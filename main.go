// Command ledger-api-go serves a minimal double-entry ledger over HTTP.
// It is intentionally thin: all bookkeeping logic lives in the ledger
// package; this file only assembles the HTTP transport.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

type server struct {
	ledger *ledger.Ledger
	// tenants is the multi-tenant registry (see ledger.TenantStore):
	// the default ledger above is adopted as the default tenant, and
	// every request that names a tenant (via the /tenants/{id} path
	// prefix or the x-tenant header) resolves to that tenant's own
	// isolated ledger through ledgerFor. Background workers (hold,
	// review, schedule, freeze sweepers, snapshot backups, the
	// balance-history recorder) keep operating on the default tenant's
	// ledger: per-tenant background schedules are out of scope.
	tenants      *ledger.TenantStore
	metrics      *Metrics
	maxBodyBytes int64
	// balanceHistoryDir is the directory the balance-history recorder
	// writes to (LEDGER_BALANCE_HISTORY_DIR). Empty means unconfigured,
	// and GET /balance-history answers 404 fail-closed.
	balanceHistoryDir string
	// snapshotBackupDir is the directory the snapshot backup worker
	// writes to (LEDGER_SNAPSHOT_BACKUP_DIR). Empty means unconfigured,
	// and GET /snapshots/diff answers 404 fail-closed.
	snapshotBackupDir string
	// backupKeys is the parsed LEDGER_BACKUP_ENCRYPTION_KEY ring
	// (LG-77): GET /snapshots/diff decrypts encrypted backups with
	// it. Nil when no key is configured — plaintext files load fine,
	// encrypted ones fail closed.
	backupKeys map[string][32]byte
	// auditSink forwards outbox domain events to an external webhook
	// (LEDGER_AUDIT_SINK_URL). Nil when disabled; GET /metrics mirrors
	// its counters when present.
	auditSink *ledger.AuditSink
	// webhooks is the outbound signed-webhook dispatcher
	// (LEDGER_WEBHOOKS, LG-102). Nil when disabled; GET
	// /webhooks/deliveries 404s fail-closed and GET /metrics mirrors
	// its failure counter when present.
	webhooks *ledger.WebhookDispatcher
}

type createEntryRequest struct {
	DebitAccount   ledger.AccountID  `json:"debit_account"`
	CreditAccount  ledger.AccountID  `json:"credit_account"`
	AmountCents    int64             `json:"amount_cents"`
	Currency       string            `json:"currency"`
	IdempotencyKey string            `json:"idempotency_key"`
	Memo           string            `json:"memo"`
	ExternalRef    string            `json:"external_ref"`
	Attachments    []string          `json:"attachments"`
	Metadata       map[string]string `json:"metadata"`
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ledger-api-go: failed to generate entry ID: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleCreateEntry implements POST /entries.
// The server generates ID and CreatedAt when the client omits them.
// A first-time post returns 201; a duplicate idempotency key returns 200
// with the originally posted entry; invalid entries (including a malformed
// currency code) return 400; a post through a frozen account returns 403;
// a post that would overdraw an overdraft-protected account returns 422,
// as does a post that would take the payer's daily outflow above its
// configured limit.
// The currency field is optional and defaults to USD; when given it must
// be a 3-letter uppercase ISO 4217 code.
// writePostError maps a Post-family booking rejection to its HTTP
// status and rejection metric. It is shared by POST /entries and the
// mandate-debit endpoint: both book through the same postLocked gate
// sequence, so a frozen/overdraft/period rejection means the same thing
// on either. Every recognized booking error is terminal here —
// including ErrAccountClosed, whose original inline copy in
// handleCreateEntry was missing its return and fell through to a second
// (ignored) write.
func (s *server) writePostError(w http.ResponseWriter, err error) {
	if errors.Is(err, ledger.ErrAccountClosed) {
		s.metrics.ClosedRejections.Add(1)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ledger.ErrAccountFrozen) {
		s.metrics.FrozenRejections.Add(1)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ledger.ErrAccountSuspended) {
		s.metrics.SuspendedRejections.Add(1)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ledger.ErrAccountOverdraft) {
		s.metrics.OverdraftRejections.Add(1)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ledger.ErrVANNoDebit) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ledger.ErrCreditLimitExceeded) {
		s.metrics.CreditLimitRejections.Add(1)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ledger.ErrAccountDirectionMismatch) {
		s.metrics.ChartDirectionRejections.Add(1)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ledger.ErrDailyLimitExceeded) {
		s.metrics.DailyLimitRejections.Add(1)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ledger.ErrBudgetExceeded) {
		s.metrics.BudgetRejections.Add(1)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ledger.ErrAccountDenied) {
		s.metrics.DenylistRejections.Add(1)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ledger.ErrPeriodClosed) {
		s.metrics.PeriodRejections.Add(1)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ledger.ErrInvalidCurrency) {
		s.metrics.CurrencyRejections.Add(1)
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

// writeMandateError maps a mandate-domain error to its HTTP status: 404
// for an unknown mandate, 422 for semantic authorization rejections
// (the request is well-formed, the mandate just does not cover it),
// and reports whether err was a mandate-domain error at all.
func writeMandateError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, ledger.ErrMandateNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return true
	}
	switch {
	case errors.Is(err, ledger.ErrMandateNotActive),
		errors.Is(err, ledger.ErrMandateExpired),
		errors.Is(err, ledger.ErrMandateAmountExceeded),
		errors.Is(err, ledger.ErrMandateCurrencyMismatch),
		errors.Is(err, ledger.ErrMandateFrequencyExceeded):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return true
	}
	return false
}

func (s *server) handleCreateEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()

	// Every POST attempt is counted; replays are counted separately below.
	s.metrics.PostsTotal.Add(1)

	// Bound the body so a single POST cannot exhaust server memory, and
	// decode strictly: unknown fields fail fast (surfacing client typos
	// instead of silently dropping them), and trailing garbage after the
	// JSON value is rejected.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req createEntryRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return
	}

	entry := ledger.JournalEntry{
		ID:             newID(),
		DebitAccount:   req.DebitAccount,
		CreditAccount:  req.CreditAccount,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		IdempotencyKey: req.IdempotencyKey,
		Memo:           req.Memo,
		ExternalRef:    req.ExternalRef,
		Attachments:    req.Attachments,
		Metadata:       req.Metadata,
		CreatedAt:      time.Now(),
	}

	posted, duplicate, err := s.ledgerFor(r).Post(entry)
	if err != nil {
		s.writePostError(w, err)
		return
	}
	if duplicate {
		s.metrics.IdempotencyHits.Add(1)
		writeJSON(w, http.StatusOK, posted)
		return
	}
	writeJSON(w, http.StatusCreated, posted)
}

// handleDryRunEntry implements POST /entries/dry-run, the what-if twin of
// POST /entries: the request body is decoded exactly like a real posting
// and the ledger runs the full Post check sequence (field validation,
// idempotency replay, frozen, overdraft, period, and daily-limit checks)
// against a private deep copy of its state, then discards the copy. The
// response reports what the real call would do: the same status code the
// real POST would return (201-shape as 200 with would_succeed=true, or
// the rejection status), and on success the per-leg balance deltas and
// the version bracket. Nothing is recorded — no journal row, no chain
// link, no version bump, no idempotency-key registration, no audit event.
func (s *server) handleDryRunEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()

	s.metrics.DryRunRequests.Add(1)

	// Same transport contract as POST /entries: bounded body, strict
	// decoding, no trailing garbage.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req createEntryRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return
	}

	entry := ledger.JournalEntry{
		ID:             newID(),
		DebitAccount:   req.DebitAccount,
		CreditAccount:  req.CreditAccount,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		IdempotencyKey: req.IdempotencyKey,
		Memo:           req.Memo,
		ExternalRef:    req.ExternalRef,
		Attachments:    req.Attachments,
		Metadata:       req.Metadata,
		CreatedAt:      time.Now(),
	}

	result, err := s.ledgerFor(r).DryRunPost(entry)
	if err != nil {
		// The same status the real POST /entries would return, so a
		// client can swap the /dry-run suffix for the real endpoint and
		// keep its error handling unchanged.
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditLimitExceeded) {
			s.metrics.CreditLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountDirectionMismatch) {
			s.metrics.ChartDirectionRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrDailyLimitExceeded) {
			s.metrics.DailyLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBudgetExceeded) {
			s.metrics.BudgetRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountDenied) {
			s.metrics.DenylistRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			s.metrics.CurrencyRejections.Add(1)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type createTransferRequest struct {
	TransferID     string            `json:"transfer_id"`
	FromAccount    ledger.AccountID  `json:"from_account"`
	ToAccount      ledger.AccountID  `json:"to_account"`
	AmountCents    int64             `json:"amount_cents"`
	Currency       string            `json:"currency"`
	ToCurrency     string            `json:"to_currency"`
	FXAccount      ledger.AccountID  `json:"fx_account"`
	FeeCents       int64             `json:"fee_cents"`
	FeeAccount     ledger.AccountID  `json:"fee_account"`
	FeeSplits      []ledger.FeeSplit `json:"fee_splits,omitempty"`
	SkipFee        bool              `json:"skip_fee"`
	IdempotencyKey string            `json:"idempotency_key"`
	Memo           string            `json:"memo"`
	ExternalRef    string            `json:"external_ref"`
	Attachments    []string          `json:"attachments"`
	Metadata       map[string]string `json:"metadata"`
	// MaxSlippageBps is the opt-in LG-86 slippage cap for
	// cross-currency transfers, in basis points (see
	// ledger.Transfer.MaxSlippageBps). It is plumbed through to both
	// POST /transfers and POST /transfers/dry-run.
	MaxSlippageBps int64 `json:"max_slippage_bps"`
	// FXSide selects the dealing price of a cross-currency transfer
	// (see ledger.Transfer.FXSide, LG-93): "sell" (the default) converts
	// at the pair's bid, "buy" at the ask. Anything else is a 400.
	FXSide string `json:"fx_side"`
	// FXIncomeAccount overrides the ledger's FX spread-income account
	// for this transfer (see ledger.Transfer.FXIncomeAccount, LG-93):
	// the credit counterparty of the "<transfer_id>/spread" leg.
	FXIncomeAccount ledger.AccountID `json:"fx_income_account"`
}

// handleCreateTransfer implements POST /transfers, the payment-domain view
// of a posting: the caller names the payer (from_account) and the payee
// (to_account) and the ledger books the double-entry pair atomically —
// either every leg lands or nothing does. An optional fee leg
// (fee_cents + fee_account, or the server-wide LEDGER_TRANSFER_FEE policy
// unless skip_fee is set) charges the payer on top of the amount and books
// it to the fee account as a second entry; the receipt's fee_cents reports
// what was booked. fee_splits (instead of fee_account) divides the fee
// across several recipients in basis points — see ledger/fee_split.go —
// and the receipt discloses each recipient's share.
//
// The server generates transfer_id when the client omits it. A first-time
// transfer returns 201 with the receipt; a duplicate idempotency key
// returns 200 with the originally posted receipt; invalid transfers return
// 400; a transfer through a frozen account returns 403; a transfer that
// would overdraw an overdraft-protected payer returns 422, as does a
// transfer that would take the payer's daily outflow (amount + fee) above
// its configured limit. A transfer at or above the payer's review
// threshold (see LEDGER_REVIEW_THRESHOLD) does not settle: it returns 201
// with a receipt carrying review_status "pending_review" — its legs are
// journaled but marked pending and the payer's funds are frozen — and an
// operator settles or releases it with POST /transfers/{id}/approve or
// POST /transfers/{id}/reject. A transfer whose payer has a transfer
// beneficiary allowlist installed and names a payee not on it is
// rejected with 403 (see PUT /accounts/{id}/allowlist). A cross-currency
// transfer (to_currency set and different from currency) converts at the
// configured rate for the pair
// (see LEDGER_FX_RATES): the payee settles in to_currency, the payer pays
// in currency, and the receipt discloses the conversion in its fx field;
// a pair with no configured rate is rejected with 422. The currency field is
// optional and defaults to USD; when given it must be a 3-letter uppercase
// ISO 4217 code, and the fee leg is booked in the source currency.
func (s *server) handleCreateTransfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()

	// Every POST attempt is counted; replays are counted separately below.
	s.metrics.TransfersTotal.Add(1)

	// Same transport contract as POST /entries: bounded body, strict
	// decoding, no trailing garbage.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req createTransferRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return
	}

	id := req.TransferID
	if id == "" {
		id = newID()
	}
	transfer := ledger.Transfer{
		ID:              id,
		From:            req.FromAccount,
		To:              req.ToAccount,
		AmountCents:     req.AmountCents,
		Currency:        req.Currency,
		ToCurrency:      req.ToCurrency,
		FXAccount:       req.FXAccount,
		FeeCents:        req.FeeCents,
		FeeAccount:      req.FeeAccount,
		FeeSplits:       req.FeeSplits,
		SkipFee:         req.SkipFee,
		IdempotencyKey:  req.IdempotencyKey,
		Memo:            req.Memo,
		ExternalRef:     req.ExternalRef,
		Attachments:     req.Attachments,
		Metadata:        req.Metadata,
		MaxSlippageBps:  req.MaxSlippageBps,
		FXSide:          req.FXSide,
		FXIncomeAccount: req.FXIncomeAccount,
		CreatedAt:       time.Now(),
	}
	if req.ToCurrency != "" {
		s.metrics.FXTransfersTotal.Add(1)
	}

	receipt, err := s.ledgerFor(r).PostTransfer(transfer)
	if err != nil {
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBeneficiaryNotAllowed) {
			s.metrics.AllowlistRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrVANNoDebit) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditLimitExceeded) {
			s.metrics.CreditLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrDailyLimitExceeded) {
			s.metrics.DailyLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBudgetExceeded) {
			s.metrics.BudgetRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountDenied) {
			s.metrics.DenylistRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCrossCurrencyTransfer) {
			s.metrics.CurrencyRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrFXRateMissing) {
			s.metrics.CurrencyRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrFXRateExpired) {
			s.metrics.CurrencyRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrFXSlippageExceeded) {
			s.metrics.CurrencyRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidMaxSlippage) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrFXAccountNotConfigured) || errors.Is(err, ledger.ErrInvalidFXAccount) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidFXSide) || errors.Is(err, ledger.ErrFXIncomeAccountNotConfigured) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			s.metrics.CurrencyRejections.Add(1)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if receipt.Duplicate {
		s.metrics.TransferIdempotencyHits.Add(1)
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	if receipt.FeeCents > 0 {
		s.metrics.TransferFeeCentsTotal.Add(uint64(receipt.FeeCents))
	}
	// First-time freezes are counted; idempotent replays of the review
	// receipt are counted as idempotency hits above, not here.
	if receipt.ReviewStatus == ledger.ReviewStatusPending {
		s.metrics.ReviewPendingTotal.Add(1)
	}
	writeJSON(w, http.StatusCreated, receipt)
}

// reviewDecisionRequest is the body of the operator review endpoints:
// who is deciding, and (for rejections) why. The reviewer identity is
// recorded on the review and in the structured audit log's hash chain,
// so the "who approved what" trail is tamper-evident; it defaults to
// "operator" when omitted.
type reviewDecisionRequest struct {
	Reviewer string `json:"reviewer"`
	Reason   string `json:"reason"`
}

// handleApproveTransferReview implements POST /transfers/{id}/approve,
// the settlement half of the dual-control review: it flips the frozen
// legs to effective and commits them exactly like a direct POST
// /transfers — balances apply, the version bumps, chain links land — and
// returns the settlement receipt with review_status "approved".
//
// While an N-of-M quorum policy is active (see
// ledger.SetReviewQuorumPolicy), the call records one named approve
// vote instead: the receipt carries review_status "pending_review" and
// the live vote tally until N distinct approvers have voted, and the
// Nth vote settles. A vote by a non-member returns 403; voting the
// opposite way after voting returns 400.
//
// Approval is idempotent: re-approving returns 200 with the original
// receipt (duplicate == true) and settles nothing new. An unknown
// transfer returns 404; approving a rejected or expired review returns
// 409; a review whose expiry passed is auto-rejected and the approval
// refused with 422; a leg through a frozen account returns 403; an
// overdraft-protected payer that can no longer cover the outflow, or
// legs dated in a now-closed period, return 422 and leave the review
// pending.
func (s *server) handleApproveTransferReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "transfer id required"})
		return
	}
	var req reviewDecisionRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	receipt, err := s.ledgerFor(r).ApproveTransferReviewCtx(actx, id, req.Reviewer)
	if err != nil {
		if errors.Is(err, ledger.ErrReviewNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrReviewNotPending) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditLimitExceeded) {
			s.metrics.CreditLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrReviewExpired) {
			s.metrics.ReviewRejectedTotal.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrQuorumNotMember) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrReviewerAlreadyVoted) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// LG-124: under a quorum policy the call records a named approve
	// vote. The vote is counted on the quorum vote metric with the
	// outcome the ledger reports (recorded / settled / duplicate); the
	// idempotent replay of an already-settled review carries no tally
	// and counts nothing new.
	if t := receipt.QuorumTally; t != nil {
		s.metrics.QuorumVotesTotal.Add(t.LastVoter, "approve", t.Outcome)
	}
	if !receipt.Duplicate && receipt.ReviewStatus == ledger.ReviewStatusApproved {
		s.metrics.ReviewApprovedTotal.Add(1)
		if receipt.FeeCents > 0 {
			s.metrics.TransferFeeCentsTotal.Add(uint64(receipt.FeeCents))
		}
	}
	writeJSON(w, http.StatusOK, receipt)
}

// handleRejectTransferReview implements POST /transfers/{id}/reject:
// drops a pending review without settling anything, releasing the frozen
// funds back to the payer's available balance. The legs stay in the
// journal as historical pending-marked rows.
//
// While an N-of-M quorum policy is active (see
// ledger.SetReviewQuorumPolicy), the call records one named reject
// vote: with a RejectDecisive policy the vote rejects at once, without
// it the vote is recorded as dissent and the review stays pending. A
// vote by a non-member returns 403; voting the opposite way after
// voting returns 400.
//
// Rejection is idempotent: rejecting an already-rejected or expired
// review returns 200 with the review as-is. An unknown transfer returns
// 404; rejecting an approved review returns 409 — a settled transfer
// cannot be un-settled through review.
func (s *server) handleRejectTransferReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "transfer id required"})
		return
	}
	var req reviewDecisionRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	review, changed, err := s.ledgerFor(r).RejectTransferReviewCtx(actx, id, req.Reviewer, req.Reason)
	if err != nil {
		if errors.Is(err, ledger.ErrReviewNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrReviewNotPending) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrQuorumNotMember) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrReviewerAlreadyVoted) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// LG-124: under a quorum policy the call records a named reject
	// vote. A recorded vote (the review stays pending under a
	// non-decisive policy) counts on the quorum vote metric but not on
	// the rejection total — only a review that actually closed as
	// rejected feeds ledger_review_rejected_total.
	if _, ok := s.ledgerFor(r).ReviewQuorumPolicy(); ok {
		voter := req.Reviewer
		if voter == "" {
			// Mirrors the ledger's defaultReviewOperator: the vote was
			// recorded under the defaulted identity.
			voter = "operator"
		}
		if changed || review.Status == ledger.ReviewStatusPending {
			outcome := ledger.QuorumOutcomeDuplicate
			if changed {
				outcome = ledger.QuorumOutcomeRecorded
				if review.Status == ledger.ReviewStatusRejected {
					outcome = ledger.QuorumOutcomeDecisive
				}
			}
			s.metrics.QuorumVotesTotal.Add(voter, "reject", outcome)
		}
		if changed && review.Status == ledger.ReviewStatusRejected {
			s.metrics.ReviewRejectedTotal.Add(1)
		}
	} else if changed {
		s.metrics.ReviewRejectedTotal.Add(1)
	}
	writeJSON(w, http.StatusOK, review)
}

// handleExpireReviews implements POST /transfers/reviews/expire: marks
// every pending review whose expiry has passed as expired, releasing its
// frozen funds, and reports how many were marked. It is the manual
// version of the background review sweeper (see
// LEDGER_REVIEW_SWEEP_INTERVAL); both paths run the same ExpireReviews
// sweep and feed ledger_review_rejected_total.
func (s *server) handleExpireReviews(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	expired := s.ledgerFor(r).ExpireReviews()
	s.metrics.ReviewRejectedTotal.Add(uint64(expired))
	writeJSON(w, http.StatusOK, map[string]any{"expired": expired})
}

// handleDryRunTransfer implements POST /transfers/dry-run, the what-if
// twin of POST /transfers: the request body is decoded exactly like a real
// transfer and the ledger runs the full PostTransfer check sequence —
// field validation, fee resolution, FX conversion, idempotency replay,
// frozen/overdraft/daily-limit/period risk checks — against a private deep
// copy of its state, then discards the copy. The response reports what the
// real call would do: the same status code the real POST would return, and
// on success the per-leg balance deltas and the version bracket. Nothing
// is recorded.
func (s *server) handleDryRunTransfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()

	s.metrics.DryRunRequests.Add(1)

	// Same transport contract as POST /transfers: bounded body, strict
	// decoding, no trailing garbage.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req createTransferRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return
	}

	id := req.TransferID
	if id == "" {
		id = newID()
	}
	transfer := ledger.Transfer{
		ID:              id,
		From:            req.FromAccount,
		To:              req.ToAccount,
		AmountCents:     req.AmountCents,
		Currency:        req.Currency,
		ToCurrency:      req.ToCurrency,
		FXAccount:       req.FXAccount,
		FeeCents:        req.FeeCents,
		FeeAccount:      req.FeeAccount,
		FeeSplits:       req.FeeSplits,
		SkipFee:         req.SkipFee,
		IdempotencyKey:  req.IdempotencyKey,
		Memo:            req.Memo,
		ExternalRef:     req.ExternalRef,
		Attachments:     req.Attachments,
		Metadata:        req.Metadata,
		MaxSlippageBps:  req.MaxSlippageBps,
		FXSide:          req.FXSide,
		FXIncomeAccount: req.FXIncomeAccount,
		CreatedAt:       time.Now(),
	}

	result, err := s.ledgerFor(r).DryRunTransfer(transfer)
	if err != nil {
		// The same status the real POST /transfers would return.
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBeneficiaryNotAllowed) {
			s.metrics.AllowlistRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditLimitExceeded) {
			s.metrics.CreditLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrDailyLimitExceeded) {
			s.metrics.DailyLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBudgetExceeded) {
			s.metrics.BudgetRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountDenied) {
			s.metrics.DenylistRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCrossCurrencyTransfer) {
			s.metrics.CurrencyRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrFXRateMissing) {
			s.metrics.CurrencyRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrFXRateExpired) {
			s.metrics.CurrencyRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrFXSlippageExceeded) {
			s.metrics.CurrencyRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidMaxSlippage) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrFXAccountNotConfigured) || errors.Is(err, ledger.ErrInvalidFXAccount) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidFXSide) || errors.Is(err, ledger.ErrFXIncomeAccountNotConfigured) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			s.metrics.CurrencyRejections.Add(1)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type batchEntryRequest struct {
	EntryID        string            `json:"entry_id"`
	DebitAccount   ledger.AccountID  `json:"debit_account"`
	CreditAccount  ledger.AccountID  `json:"credit_account"`
	AmountCents    int64             `json:"amount_cents"`
	Currency       string            `json:"currency"`
	IdempotencyKey string            `json:"idempotency_key"`
	Memo           string            `json:"memo"`
	ExternalRef    string            `json:"external_ref"`
	Attachments    []string          `json:"attachments"`
	Metadata       map[string]string `json:"metadata"`
}

type createBatchRequest struct {
	BatchID        string              `json:"batch_id"`
	Entries        []batchEntryRequest `json:"entries"`
	IdempotencyKey string              `json:"idempotency_key"`
}

// handleCreateBatch implements POST /entries/batch, the bulk-settlement
// view of a posting: the caller submits several double-entry legs at once
// (a payroll run, a merchant batch settlement) and the ledger commits them
// atomically — either every new entry lands or nothing does. One bad leg
// rejects the whole batch; a batch through a frozen account, or one that
// would overdraw an overdraft-protected payer or breach a daily outflow
// limit, is rejected the same way.
//
// The server generates batch_id when the client omits it, and generates
// entry IDs for entries that omit them. A first-time batch returns 201
// with the receipt; a duplicate batch idempotency key returns 200 with the
// originally posted receipt; invalid batches return 400; a batch through
// a frozen account returns 403; a batch that would overdraw a protected
// account or breach a daily limit returns 422.
func (s *server) handleCreateBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()

	// Every POST attempt is counted; replays are counted separately below.
	s.metrics.BatchTotal.Add(1)

	// Same transport contract as POST /entries: bounded body, strict
	// decoding, no trailing garbage.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req createBatchRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return
	}

	id := req.BatchID
	if id == "" {
		id = newID()
	}
	entries := make([]ledger.JournalEntry, 0, len(req.Entries))
	for _, re := range req.Entries {
		entryID := re.EntryID
		if entryID == "" {
			entryID = newID()
		}
		entries = append(entries, ledger.JournalEntry{
			ID:             entryID,
			DebitAccount:   re.DebitAccount,
			CreditAccount:  re.CreditAccount,
			AmountCents:    re.AmountCents,
			Currency:       re.Currency,
			IdempotencyKey: re.IdempotencyKey,
			Memo:           re.Memo,
			ExternalRef:    re.ExternalRef,
			Attachments:    re.Attachments,
			Metadata:       re.Metadata,
			CreatedAt:      time.Now(),
		})
	}
	batch := ledger.Batch{
		ID:             id,
		Entries:        entries,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      time.Now(),
	}

	receipt, err := s.ledgerFor(r).PostBatch(batch)
	if err != nil {
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditLimitExceeded) {
			s.metrics.CreditLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountDirectionMismatch) {
			s.metrics.ChartDirectionRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrDailyLimitExceeded) {
			s.metrics.DailyLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBudgetExceeded) {
			s.metrics.BudgetRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountDenied) {
			s.metrics.DenylistRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			s.metrics.CurrencyRejections.Add(1)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if receipt.Duplicate {
		s.metrics.BatchIdempotencyHits.Add(1)
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	writeJSON(w, http.StatusCreated, receipt)
}

type createSweepRequest struct {
	SweepID        string             `json:"sweep_id"`
	FromAccounts   []ledger.AccountID `json:"from_accounts"`
	ToAccount      ledger.AccountID   `json:"to_account"`
	IdempotencyKey string             `json:"idempotency_key"`
}

// handleCreateSweep implements POST /sweeps, the treasury view of the
// payment domain: the caller names the source accounts (typically a
// merchant's sub-merchants, see GET /accounts/{id}/rollup) and the target
// account, and the ledger atomically moves every source's positive
// per-currency balance to the target — one journal entry per
// (source, currency), all under the sweep ID. Sweeps reuse transfer
// semantics but never charge a fee: they are internal treasury movements,
// so the LEDGER_TRANSFER_FEE policy does not apply.
//
// The server generates sweep_id when the client omits it. A first-time
// sweep returns 201 with the receipt; a duplicate idempotency key returns
// 200 with the originally posted receipt; invalid sweeps return 400; a
// sweep touching a frozen account returns 403. A sweep whose sources all
// have non-positive balances succeeds with an empty receipt (sources are
// skipped, never swept into debt).
func (s *server) handleCreateSweep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	// Every POST attempt is counted; replays are counted separately below.
	s.metrics.SweepsTotal.Add(1)

	var req createSweepRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}

	id := req.SweepID
	if id == "" {
		id = newID()
	}
	receipt, err := s.ledgerFor(r).PostSweep(ledger.Sweep{
		ID:             id,
		From:           req.FromAccounts,
		To:             req.ToAccount,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      time.Now(),
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBudgetExceeded) {
			s.metrics.BudgetRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if receipt.Duplicate {
		s.metrics.SweepIdempotencyHits.Add(1)
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	writeJSON(w, http.StatusCreated, receipt)
}

// handleDryRunSweep implements POST /sweeps/dry-run, the what-if twin of
// POST /sweeps: the request body is decoded exactly like a real sweep and
// the ledger runs the full PostSweep check sequence — field validation,
// leg planning against current balances, idempotency replay, ID
// conflicts, frozen checks, and the period gate — against a private deep
// copy of its state, then discards the copy. The response reports what the
// real call would do: the same status code the real POST would return, and
// on success the per-leg balance deltas and the version bracket. Nothing
// is recorded.
func (s *server) handleDryRunSweep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	s.metrics.DryRunRequests.Add(1)

	var req createSweepRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}

	id := req.SweepID
	if id == "" {
		id = newID()
	}
	result, err := s.ledgerFor(r).DryRunSweep(ledger.Sweep{
		ID:             id,
		From:           req.FromAccounts,
		To:             req.ToAccount,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      time.Now(),
	})
	if err != nil {
		// The same status the real POST /sweeps would return.
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBudgetExceeded) {
			s.metrics.BudgetRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type createMergeRequest struct {
	MergeID        string           `json:"merge_id"`
	FromAccount    ledger.AccountID `json:"from_account"`
	ToAccount      ledger.AccountID `json:"to_account"`
	IdempotencyKey string           `json:"idempotency_key"`
}

// handleCreateMerge implements POST /merges, the account-lifecycle view
// of the payment domain: the caller names a source account to
// decommission (a merchant entity that changed hands, a consolidated
// sub-account) and the target account that absorbs it. The ledger
// atomically moves the source's every currency balance to the target —
// one journal entry per currency, "<merge ID>/<currency>" — and freezes
// the source in the same commit, so the decommissioned account can never
// move money again.
//
// The server generates merge_id when the client omits it. A first-time
// merge returns 201 with the receipt; a duplicate idempotency key returns
// 200 with the originally posted receipt (even though the source is now
// frozen — the replay books nothing new); invalid merges return 400; a
// merge touching a frozen account returns 403; a merge that would
// overdraw an overdraft-protected target (only possible when the source
// carries negative balances, which the target absorbs as debt) returns
// 422. A merge whose source held no balances still succeeds and still
// freezes the source — the freeze is the point — with an empty leg list.
func (s *server) handleCreateMerge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	// Every POST attempt is counted; replays are counted separately below.
	s.metrics.MergesTotal.Add(1)

	var req createMergeRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}

	id := req.MergeID
	if id == "" {
		id = newID()
	}
	receipt, err := s.ledgerFor(r).PostMerge(ledger.Merge{
		ID:             id,
		From:           req.FromAccount,
		To:             req.ToAccount,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      time.Now(),
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditLimitExceeded) {
			s.metrics.CreditLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if receipt.Duplicate {
		s.metrics.MergeIdempotencyHits.Add(1)
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	writeJSON(w, http.StatusCreated, receipt)
}

// createReversalRequest is the body of POST /entries/{id}/reverse. The
// idempotency key is optional: the reversal ID itself
// ("<entry ID>/reversal") is deterministic, so a keyless retry returns the
// existing reversal with 200. A *new* key on an already-reversed entry
// signals a distinct reversal intent and is rejected with 409.
type createReversalRequest struct {
	IdempotencyKey string            `json:"idempotency_key"`
	Metadata       map[string]string `json:"metadata"`
}

// handleReverseEntry implements POST /entries/{id}/reverse, the
// accounting correction for a posted journal entry (冲正): it appends a
// new entry with the debit/credit legs swapped, netting the original's
// balance effects to zero. The original entry is never modified — the
// audit chain stays append-only.
//
// A first-time reversal returns 201 with the reversal entry. Repeating
// the request (no key, or the same idempotency key) returns 200 with the
// originally created reversal, even if the involved accounts were frozen
// afterwards — the replay books nothing new. A new idempotency key on an
// already-reversed entry returns 409 (the deterministic ID cannot express
// a second reversal of the same entry). An unknown entry ID returns 404;
// a pending-review entry returns 409 (approve or reject the review
// instead); a frozen account returns 403; overdraft, daily-limit, and
// closed-period rejections return 422; anything else is a 400.
func (s *server) handleReverseEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req createReversalRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}

	entryID := r.PathValue("id")
	reversal, duplicate, err := s.ledgerFor(r).ReverseEntryWithMetadata(entryID, req.IdempotencyKey, req.Metadata)
	if err != nil {
		if errors.Is(err, ledger.ErrEntryNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAlreadyReversed) || errors.Is(err, ledger.ErrReversalOfPendingReview) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditLimitExceeded) {
			s.metrics.CreditLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrDailyLimitExceeded) {
			s.metrics.DailyLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBudgetExceeded) {
			s.metrics.BudgetRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, reversal)
		return
	}
	// ledger_reversals_total{kind="full"} counts committed full reversal
	// entries, not requests: idempotent replays book nothing new and
	// are not counted twice.
	s.metrics.ReversalsFullTotal.Add(1)
	writeJSON(w, http.StatusCreated, reversal)
}

// createPartialReversalRequest is the body of
// POST /entries/{id}/reverse-partial. amount_cents is the slice of the
// original entry's amount to reverse back; the idempotency key is
// optional, like POST /entries/{id}/reverse.
type createPartialReversalRequest struct {
	AmountCents    int64  `json:"amount_cents"`
	IdempotencyKey string `json:"idempotency_key"`
}

// handleReverseEntryPartial implements POST /entries/{id}/reverse-partial,
// the partial accounting correction (部分冲正): it appends a new entry
// reversing amount_cents of the original entry's amount, with the
// debit/credit legs swapped. The original entry is never modified — the
// audit chain stays append-only.
//
// The partial reversal ID is deterministic ("<entry ID>/reversal/<n>",
// n the 1-based index of this entry's n-th partial reversal): a
// first-time partial reversal returns 201 with the reversal entry, and
// repeating the request with the same idempotency key returns 200 with
// the originally created reversal. Keyless requests always book the
// next partial index — the deterministic ID is per-index, so a keyless
// request cannot identify which partial to replay. The cumulative corrected
// amount (partial reversals plus a full reversal, which also counts)
// can never exceed the original amount: anything over the cap returns
// 409, as does a full reversal once any partial reversal exists (it
// would over-correct — reverse the partials individually instead).
//
// An unknown entry ID returns 404; a non-positive amount_cents returns
// 400; a pending-review entry returns 409 (approve or reject the review
// instead); frozen, closed, and suspended accounts return 403;
// overdraft, daily-limit, and closed-period rejections return 422.
func (s *server) handleReverseEntryPartial(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req createPartialReversalRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}

	entryID := r.PathValue("id")
	reversal, duplicate, err := s.ledgerFor(r).ReverseEntryPartial(entryID, req.AmountCents, req.IdempotencyKey)
	if err != nil {
		if errors.Is(err, ledger.ErrEntryNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAlreadyReversed) || errors.Is(err, ledger.ErrReversalOfPendingReview) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrDailyLimitExceeded) {
			s.metrics.DailyLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, reversal)
		return
	}
	// ledger_reversals_total{kind="partial"} counts committed partial
	// reversal entries, not requests: idempotent replays book nothing
	// new and are not counted twice.
	s.metrics.ReversalsPartialTotal.Add(1)
	writeJSON(w, http.StatusCreated, reversal)
}

// handleEntryLinks implements GET /entries/{id}/links, the bidirectional
// entry-link query (see ledger.EntryLinks, LG-123): every declared link
// touching the entry — the reversal_of links the reversal paths declare
// automatically, plus any adjustment_of/fee_of/refund_of links declared
// through ledger.DeclareEntryLink — each annotated with the direction
// the entry was found in ("outgoing" when the entry is the link's From
// endpoint, "incoming" when it is the To endpoint), in declaration
// order. An entry with no declared links returns an empty list, not
// 404; an unknown entry ID returns 404.
func (s *server) handleEntryLinks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	l := s.ledgerFor(r)
	if !l.EntryExists(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": ledger.ErrEntryNotFound.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entry_id": id,
		"links":    l.EntryLinks(id),
	})
}

// decodeRequestBody decodes a JSON request body into dst with the
// transport contract the POST handlers share: bounded body, strict
// decoding (unknown fields fail fast), no trailing garbage. It reports
// whether decoding succeeded, writing the 400/413 itself on failure.
func (s *server) decodeRequestBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return false
	}
	return true
}

// createMandateRequest is the body of POST /mandates: the standing
// direct-debit authorization for payee to pull from payer, capped per
// pull at max_amount_cents in currency, at most once per frequency
// window (per-day, per-week, per-month), between valid_from and
// valid_until (RFC3339). The id is the mandate's reference — it must be
// unused as a mandate ID and as a journal entry ID.
type createMandateRequest struct {
	ID             string    `json:"id"`
	Payer          string    `json:"payer"`
	Payee          string    `json:"payee"`
	MaxAmountCents int64     `json:"max_amount_cents"`
	Currency       string    `json:"currency"`
	Frequency      string    `json:"frequency"`
	ValidFrom      time.Time `json:"valid_from"`
	ValidUntil     time.Time `json:"valid_until"`
}

// mandateDebitRequest is the body of POST /mandates/{id}/debits: one
// pull under the mandate. The legs come from the mandate itself (debit
// the payee, credit the payer); currency empty means the mandate's
// currency. The id is the debit's journal entry ID.
type mandateDebitRequest struct {
	ID             string            `json:"id"`
	AmountCents    int64             `json:"amount_cents"`
	Currency       string            `json:"currency"`
	Memo           string            `json:"memo"`
	ExternalRef    string            `json:"external_ref"`
	Attachments    []string          `json:"attachments"`
	Metadata       map[string]string `json:"metadata"`
	IdempotencyKey string            `json:"idempotency_key"`
}

// handleCreateMandate implements POST /mandates: installs a
// direct-debit mandate (see ledger.CreateMandate, LG-125). 201 with the
// mandate; 400 on field errors or an ID already in use.
func (s *server) handleCreateMandate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req createMandateRequest
	if !s.decodeRequestBody(w, r, &req) {
		return
	}
	m, err := s.ledgerFor(r).CreateMandate(ledger.Mandate{
		ID:             req.ID,
		Payer:          ledger.AccountID(req.Payer),
		Payee:          ledger.AccountID(req.Payee),
		MaxAmountCents: req.MaxAmountCents,
		Currency:       req.Currency,
		Frequency:      req.Frequency,
		ValidFrom:      req.ValidFrom,
		ValidUntil:     req.ValidUntil,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// handleGetMandate implements GET /mandates/{id}: the mandate with its
// effective status (lazy expiry applied). 404 on an unknown mandate.
func (s *server) handleGetMandate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	m, ok := s.ledgerFor(r).GetMandate(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": ledger.ErrMandateNotFound.Error()})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleDebitMandate implements POST /mandates/{id}/debits: one pull
// under the mandate's authorization (see ledger.DebitWithMandate,
// LG-125). 201 with the receipt (200 with duplicate: true on an
// idempotent replay); 404 on an unknown mandate; 422 when the mandate
// does not cover the debit (suspended/cancelled/expired, over the
// per-debit cap, wrong currency, frequency window used); the booking
// itself runs the full Post gate sequence, so frozen/overdraft/period
// rejections map exactly like POST /entries.
func (s *server) handleDebitMandate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req mandateDebitRequest
	if !s.decodeRequestBody(w, r, &req) {
		return
	}
	receipt, err := s.ledgerFor(r).DebitWithMandate(r.PathValue("id"), ledger.MandateDebit{
		ID:             req.ID,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		Memo:           req.Memo,
		ExternalRef:    req.ExternalRef,
		Attachments:    req.Attachments,
		Metadata:       req.Metadata,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		if writeMandateError(w, err) {
			return
		}
		s.writePostError(w, err)
		return
	}
	if receipt.Duplicate {
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	writeJSON(w, http.StatusCreated, receipt)
}

// handleListMandateDebits implements GET /mandates/{id}/debits: the
// journal entries debited under the mandate, in debit order — the
// per-mandate reconciliation query. Pagination follows the entries
// listing: limit (1..1000, default 100) and cursor (the entry ID the
// page starts after). 404 on an unknown mandate.
func (s *server) handleListMandateDebits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	q := r.URL.Query()
	limit := 100
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": ledger.ErrInvalidLimit.Error()})
			return
		}
		limit = n
	}
	page, next, err := s.ledgerFor(r).MandateDebits(r.PathValue("id"), q.Get("cursor"), limit)
	if err != nil {
		if writeMandateError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mandate_id":  r.PathValue("id"),
		"debits":      page,
		"next_cursor": next,
	})
}

// handleSuspendMandate implements POST /mandates/{id}/suspend: pauses
// the mandate's authorization (see ledger.SuspendMandate). 404 on an
// unknown mandate; 422 when the mandate is not active or has expired.
func (s *server) handleSuspendMandate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	m, err := s.ledgerFor(r).SuspendMandate(r.PathValue("id"))
	if err != nil {
		if writeMandateError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleCancelMandate implements POST /mandates/{id}/cancel: terminally
// cancels the mandate (see ledger.CancelMandate). 404 on an unknown
// mandate; 422 when the mandate is not active/suspended or has expired.
func (s *server) handleCancelMandate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	m, err := s.ledgerFor(r).CancelMandate(r.PathValue("id"))
	if err != nil {
		if writeMandateError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleExpireMandates implements POST /mandates/expire: marks every
// mandate past its ValidUntil as expired (see ledger.ExpireMandates).
// Expiry is lazy by predicate everywhere else; the sweep exists for
// observability. Always 200 with the marked count.
func (s *server) handleExpireMandates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"expired": s.ledgerFor(r).ExpireMandates(),
	})
}

// createRevaluationRequest is the body of POST /revaluations. AsOf is
// RFC3339 and optional (default now); it is the measurement time — it
// timestamps the revaluation entries, keys their deterministic IDs, and
// gates the period check. BaseCurrency is required: the reporting
// currency every foreign balance is remeasured into.
type createRevaluationRequest struct {
	AsOf         string `json:"asof"`
	BaseCurrency string `json:"base_currency"`
}

// handleCreateRevaluation implements POST /revaluations, the period-end FX
// revaluation (see ledger.RevalueBalances): every non-base-currency
// balance is remeasured at the current FX rate table, and the unrealized
// gain/loss since the last revaluation is booked as ordinary journal
// entries against the configured revaluation P&L account.
//
// The run is best-effort per (account, currency), not atomic: the 200
// response carries the full report with each item's outcome (revalued,
// unchanged, skipped_*, failed), so one missing rate or frozen account
// never blocks the rest. Only actually committed entries move
// ledger_revaluations_total. A malformed base currency or asof returns
// 400; a missing revaluation P&L account configuration returns 400 with
// a message telling the operator to configure it.
func (s *server) handleCreateRevaluation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req createRevaluationRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}

	var asOf time.Time
	if req.AsOf != "" {
		var err error
		asOf, err = time.Parse(time.RFC3339, req.AsOf)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid asof: want RFC3339"})
			return
		}
	}

	report, err := s.ledgerFor(r).RevalueBalances(asOf, req.BaseCurrency)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// ledger_revaluations_total counts committed revaluation entries, not
	// runs or requests: only booked gains/losses move it.
	s.metrics.RevaluationsTotal.Add(uint64(len(report.Entries)))
	writeJSON(w, http.StatusOK, report)
}

// decodeJSONBody decodes a JSON request body with the same transport
// contract as POST /entries: bounded body, strict decoding (unknown
// fields fail fast), no trailing garbage. It returns false after writing
// the error response when decoding fails. Callers must check the HTTP
// method before calling it; it always closes the body.
type createTransferScheduleRequest struct {
	ScheduleID  string           `json:"schedule_id"`
	FromAccount ledger.AccountID `json:"from_account"`
	ToAccount   ledger.AccountID `json:"to_account"`
	AmountCents int64            `json:"amount_cents"`
	Currency    string           `json:"currency"`
	ToCurrency  string           `json:"to_currency"`
	FXAccount   ledger.AccountID `json:"fx_account"`
	FeeCents    int64            `json:"fee_cents"`
	FeeAccount  ledger.AccountID `json:"fee_account"`
	SkipFee     bool             `json:"skip_fee"`
	Memo        string           `json:"memo"`
	// Interval is a Go duration string ("24h", "168h"): the fixed cadence
	// between runs. Cron expressions are not supported.
	Interval string `json:"interval"`
	// NextRunAt is RFC3339: the nominal due time of the first run. A past
	// time fires on the next sweep.
	NextRunAt string `json:"next_run_at"`
	// EndsAt is RFC3339, optional: the plan completes once the next run
	// would fall after it.
	EndsAt         string `json:"ends_at"`
	// BusinessDayCadence opts into business-day settlement (LG-95):
	// after firing, NextRunAt advances T+N business days instead of a
	// fixed duration.
	BusinessDayCadence bool `json:"business_day_cadence"`
	IdempotencyKey string `json:"idempotency_key"`
}

// handleCreateTransferSchedule implements POST /transfer-schedules: it
// registers a recurring transfer plan (see ledger.TransferSchedule).
// A first-time schedule returns 201; a duplicate idempotency key returns
// 200 with the original schedule and duplicate=true; malformed requests
// return 400. Firing is done by the background schedule sweeper (see
// LEDGER_SCHEDULE_SWEEP_INTERVAL), not by this endpoint.
func (s *server) handleCreateTransferSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req createTransferScheduleRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}

	interval, err := time.ParseDuration(req.Interval)
	if err != nil || interval <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid interval: must be a positive Go duration string (e.g. \"24h\")"})
		return
	}
	var nextRunAt time.Time
	if req.NextRunAt != "" {
		nextRunAt, err = time.Parse(time.RFC3339, req.NextRunAt)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid next_run_at: must be RFC3339"})
			return
		}
	}
	var endsAt time.Time
	if req.EndsAt != "" {
		endsAt, err = time.Parse(time.RFC3339, req.EndsAt)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid ends_at: must be RFC3339"})
			return
		}
	}

	id := req.ScheduleID
	if id == "" {
		id = newID()
	}
	sched, duplicate, err := s.ledgerFor(r).CreateTransferSchedule(ledger.TransferSchedule{
		ID:             id,
		From:           req.FromAccount,
		To:             req.ToAccount,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		ToCurrency:     req.ToCurrency,
		FXAccount:      req.FXAccount,
		FeeCents:       req.FeeCents,
		FeeAccount:     req.FeeAccount,
		SkipFee:        req.SkipFee,
		Memo:           req.Memo,
		Interval:       interval,
		NextRunAt:      nextRunAt,
		EndsAt:         endsAt,
		IdempotencyKey: req.IdempotencyKey,
		BusinessDayCadence: req.BusinessDayCadence,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, map[string]any{"schedule": sched, "duplicate": true})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"schedule": sched, "duplicate": false})
}

// handleGetTransferFeeSplit implements GET /transfers/{id}/fee-split,
// the operator disclosure of a transfer's fee split (see
// ledger.TransferFeeSplit): the total fee and each recipient's booked
// share, in recipient order. An unknown transfer ID and a transfer that
// posted no fee split (no fee leg, or a single fee account) are both
// 404; the no-split error carries a hint explaining when fee_splits is
// present.
func (s *server) handleGetTransferFeeSplit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	detail, err := s.ledgerFor(r).TransferFeeSplit(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// handleGetTransferSchedule implements GET /transfer-schedules/{id}.
func (s *server) handleGetTransferSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	sched, err := s.ledgerFor(r).GetTransferSchedule(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sched)
}

// handleListScheduleRuns implements GET /transfer-schedules/{id}/runs:
// the schedule's run history, newest first. ?limit=N caps the rows.
func (s *server) handleListScheduleRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid limit: must be a non-negative integer"})
			return
		}
	}
	id := r.PathValue("id")
	runs, err := s.ledgerFor(r).ListScheduleRuns(id, limit)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedule_id": id, "runs": runs})
}

// handlePreviewTransferSchedule implements GET
// /transfer-schedules/{id}/preview: a read-only dry-run preview of the
// schedule's nominal firings inside ?horizon= (a Go duration string like
// "24h"; default 24h, max 8760h), each with its admission verdict from
// the full PostTransfer chain (see ledger.PreviewSchedule). Unknown
// schedule IDs are 404; unparsable or out-of-range horizons are 400.
// Nothing fires and nothing is recorded — the schedule's state and the
// ledger are untouched.
func (s *server) handlePreviewTransferSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	horizon := 24 * time.Hour
	if raw := r.URL.Query().Get("horizon"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 || d > 8760*time.Hour {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid horizon: must be a positive Go duration like \"24h\", at most 8760h"})
			return
		}
		horizon = d
	}
	id := r.PathValue("id")
	preview, err := s.ledgerFor(r).PreviewSchedule(id, horizon)
	if err != nil {
		if errors.Is(err, ledger.ErrScheduleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// scheduleTransition runs one pause/resume/cancel transition for
// POST /transfer-schedules/{id}/{pause,resume,cancel}.
func (s *server) scheduleTransition(w http.ResponseWriter, r *http.Request, action string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	var sched ledger.TransferSchedule
	var err error
	switch action {
	case "pause":
		sched, err = s.ledgerFor(r).PauseTransferSchedule(id)
	case "resume":
		sched, err = s.ledgerFor(r).ResumeTransferSchedule(id)
	case "cancel":
		sched, err = s.ledgerFor(r).CancelTransferSchedule(id)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown action"})
		return
	}
	if err != nil {
		if errors.Is(err, ledger.ErrScheduleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrScheduleBadTransition) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sched)
}

func (s *server) decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return false
	}
	return true
}

type createHoldRequest struct {
	HoldID         string           `json:"hold_id"`
	Account        ledger.AccountID `json:"account"`
	AmountCents    int64            `json:"amount_cents"`
	Currency       string           `json:"currency"`
	ExpiresAt      time.Time        `json:"expires_at"`
	IdempotencyKey string           `json:"idempotency_key"`
}

// handleCreateHold implements POST /holds, the authorization half of the
// auth/capture flow: it reserves amount_cents of the account's available
// funds until expires_at (RFC3339) without moving any money through the
// journal. While the hold is active, GET /accounts/{id}/balance reports
// available_cents = balance_cents - held. The server generates hold_id
// when the client omits it.
//
// A first-time hold returns 201; a duplicate idempotency key returns 200
// with the original hold; malformed requests return 400; a hold on a
// frozen account returns 403; a hold the account's available funds cannot
// cover returns 422. The currency field is optional and defaults to USD;
// when given it must be a 3-letter uppercase ISO 4217 code. expires_at is
// required — authorizations are always time-bound.
func (s *server) handleCreateHold(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.metrics.HoldsTotal.Add(1)
	var req createHoldRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	id := req.HoldID
	if id == "" {
		id = newID()
	}
	held, duplicate, err := s.ledgerFor(r).Hold(ledger.Hold{
		ID:             id,
		Account:        req.Account,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		ExpiresAt:      req.ExpiresAt,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      time.Now(),
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInsufficientAvailableFunds) {
			s.metrics.HoldRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			s.metrics.CurrencyRejections.Add(1)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if duplicate {
		s.metrics.HoldIdempotencyHits.Add(1)
		writeJSON(w, http.StatusOK, held)
		return
	}
	writeJSON(w, http.StatusCreated, held)
}

type captureHoldRequest struct {
	CaptureID      string            `json:"capture_id"`
	ToAccount      ledger.AccountID  `json:"to_account"`
	AmountCents    int64             `json:"amount_cents"`
	IdempotencyKey string            `json:"idempotency_key"`
	ExternalRef    string            `json:"external_ref"`
	Attachments    []string          `json:"attachments"`
	Metadata       map[string]string `json:"metadata"`
}

// handleCaptureHold implements POST /holds/{id}/capture, the settlement
// half of the auth/capture flow: it posts amount_cents (which must not
// exceed the held amount) as a double-entry journal entry from the held
// account to to_account, in the hold's currency, and consumes the hold —
// the un-captured remainder is released back to available funds
// automatically (see released_cents in the receipt). The server generates
// capture_id when the client omits it.
//
// A first-time capture returns 201 with the receipt; a duplicate
// idempotency key returns 200 with the original receipt; an unknown hold
// returns 404; a capture on a frozen account returns 403; a capture that
// exceeds the hold, targets a captured/released hold, targets an expired
// hold, or would overdraw an overdraft-protected held account returns 422.
func (s *server) handleCaptureHold(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hold id required"})
		return
	}
	s.metrics.CapturesTotal.Add(1)
	var req captureHoldRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	captureID := req.CaptureID
	if captureID == "" {
		captureID = newID()
	}
	receipt, err := s.ledgerFor(r).Capture(ledger.Capture{
		ID:             captureID,
		HoldID:         id,
		To:             req.ToAccount,
		AmountCents:    req.AmountCents,
		IdempotencyKey: req.IdempotencyKey,
		ExternalRef:    req.ExternalRef,
		Attachments:    req.Attachments,
		Metadata:       req.Metadata,
		CreatedAt:      time.Now(),
	})
	if err != nil {
		if errors.Is(err, ledger.ErrHoldNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditLimitExceeded) {
			s.metrics.CreditLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountDenied) {
			s.metrics.DenylistRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCaptureExceedsHold) ||
			errors.Is(err, ledger.ErrHoldNotActive) ||
			errors.Is(err, ledger.ErrHoldExpired) {
			s.metrics.HoldRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBudgetExceeded) {
			s.metrics.BudgetRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if receipt.Duplicate {
		s.metrics.CaptureIdempotencyHits.Add(1)
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	writeJSON(w, http.StatusCreated, receipt)
}

// partialCaptureHoldRequest is the body of POST
// /holds/{id}/capture/partial: settle one slice of an authorization
// hold as a real money movement. capture_seq is the caller-allocated
// 1-based sequence number of this partial capture: together with the
// hold ID it is the operation's idempotency identity, and the journal
// entry is booked under the deterministic "<holdID>/capture/<seq>" ID.
type partialCaptureHoldRequest struct {
	CaptureSeq    int               `json:"capture_seq"`
	ToAccount     ledger.AccountID  `json:"to_account"`
	AmountCents   int64             `json:"amount_cents"`
	ExternalRef   string            `json:"external_ref"`
	Attachments   []string          `json:"attachments"`
	Metadata      map[string]string `json:"metadata"`
}

// handlePartialCaptureHold implements POST /holds/{id}/capture/partial,
// the e-commerce / hotel slice of the auth/capture flow: it posts
// amount_cents (which must not exceed the hold's remaining cents) as a
// double-entry journal entry from the held account to to_account, in the
// hold's currency, under the deterministic "<holdID>/capture/<seq>"
// entry ID. The hold stays active for further partial captures until the
// cumulative captures reach the original hold amount.
//
// A first-time partial capture returns 201 with the receipt; a replay of
// the same (holdID, captureSeq) returns 200 with the original receipt;
// an unknown hold returns 404; a partial capture on a frozen account
// returns 403; a partial capture that exceeds the hold's remaining
// cents, targets a captured/released hold, targets an expired hold, or
// would overdraw an overdraft-protected held account returns 422.
func (s *server) handlePartialCaptureHold(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hold id required"})
		return
	}
	s.metrics.PartialCapturesTotal.Add(1)
	var req partialCaptureHoldRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	receipt, err := s.ledgerFor(r).CapturePartial(ledger.PartialCapture{
		HoldID:       id,
		To:           req.ToAccount,
		CaptureSeq:   req.CaptureSeq,
		AmountCents:  req.AmountCents,
		ExternalRef:  req.ExternalRef,
		Attachments:  req.Attachments,
		Metadata:     req.Metadata,
		CreatedAt:    time.Now(),
	})
	if err != nil {
		if errors.Is(err, ledger.ErrHoldNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditLimitExceeded) {
			s.metrics.CreditLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountDenied) {
			s.metrics.DenylistRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCaptureExceedsHold) ||
			errors.Is(err, ledger.ErrHoldNotActive) ||
			errors.Is(err, ledger.ErrHoldExpired) {
			s.metrics.HoldRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBudgetExceeded) {
			s.metrics.BudgetRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if receipt.Duplicate {
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	writeJSON(w, http.StatusCreated, receipt)
}

// handleReleaseHold implements POST /holds/{id}/release: drops the hold
// without settling anything, returning its reserved funds to available.
// Release is idempotent — releasing an already-released or expired hold
// is a no-op returning the hold — and works on frozen accounts (it frees
// funds rather than moving money). An unknown hold returns 404. The
// request takes no body.
func (s *server) handleReleaseHold(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hold id required"})
		return
	}
	s.metrics.ReleasesTotal.Add(1)
	h, err := s.ledgerFor(r).Release(id)
	if err != nil {
		if errors.Is(err, ledger.ErrHoldNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// handleExpireHolds implements POST /holds/expire: the operator-facing
// sweep that marks every hold whose ExpiresAt has passed as expired and
// reports how many were marked. Expiry is lazy — expired holds already
// count as inactive for available-balance purposes before the sweep — so
// this endpoint is observability and bookkeeping, not a correctness
// gate. It returns 200 {"expired": N}.
func (s *server) handleExpireHolds(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.metrics.HoldSweeps.Add(1)
	writeJSON(w, http.StatusOK, map[string]any{"expired": s.ledgerFor(r).ExpireHolds()})
}

// raiseDisputeRequest is the body of POST /disputes. dispute_id is the
// caller-chosen dispute identity (replays return the original dispute),
// transfer_id names the posted same-currency transfer being disputed,
// reason_code is one of unauthorized/not_received/duplicate/fraud,
// amount_cents is the disputed slice of the transfer (at most the
// transfer's amount — partial disputes are allowed), and currency is
// optional (it defaults to the transfer's currency and must match it
// when given).
type raiseDisputeRequest struct {
	DisputeID      string `json:"dispute_id"`
	TransferID     string `json:"transfer_id"`
	ReasonCode     string `json:"reason_code"`
	AmountCents    int64  `json:"amount_cents"`
	Currency       string `json:"currency,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// handleRaiseDispute implements POST /disputes, the payer-initiated
// chargeback-style claim against a posted transfer (see
// ledger.RaiseDispute): it freezes the disputed amount on the payee's
// available balance through an authorization hold without journaling
// anything, so raising a dispute never double-charges.
//
// A first-time raise returns 201 with the dispute. Repeating the
// dispute ID (or the idempotency key) returns 200 with the originally
// raised dispute — the replay books nothing new, even if the payee's
// account was frozen afterwards. An unknown transfer ID returns 404;
// a transfer pending dual-control review returns 409; malformed
// requests (empty IDs, non-positive amount, unknown reason code,
// amount above the transfer's, currency mismatch) return 400; a frozen
// or closed payee account returns 403; a payee whose available funds
// cannot cover the disputed amount returns 422.
func (s *server) handleRaiseDispute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req raiseDisputeRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}

	d, duplicate, err := s.ledgerFor(r).RaiseDispute(ledger.Dispute{
		ID:             req.DisputeID,
		TransferID:     req.TransferID,
		ReasonCode:     req.ReasonCode,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		if errors.Is(err, ledger.ErrDisputeTransferNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrDisputeTransferPendingReview) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInsufficientAvailableFunds) {
			s.metrics.HoldRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			s.metrics.CurrencyRejections.Add(1)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, d)
		return
	}
	// ledger_disputes_raised_total counts committed dispute raises,
	// not requests: idempotent replays book nothing new and are not
	// counted twice.
	s.metrics.DisputesRaisedTotal.Add(1)
	writeJSON(w, http.StatusCreated, d)
}

// resolveDisputeRequest is the body of POST /disputes/{id}/resolve.
// uphold == true refunds the disputed amount to the payer (the payer
// wins); uphold == false releases the freeze hold only and the payee
// keeps the funds.
type resolveDisputeRequest struct {
	Uphold bool `json:"uphold"`
}

// handleResolveDispute implements POST /disputes/{id}/resolve, the
// terminal resolution of a transfer dispute (see
// ledger.ResolveDispute): open -> resolved, irreversibly. An upheld
// dispute refunds the payer by appending a partial reversal of the
// transfer's principal entry and releases the freeze hold; a rejected
// dispute releases the hold only.
//
// A first-time resolution returns 200 with the resolved dispute. An
// unknown dispute ID returns 404; resolving an already-resolved dispute
// returns 409 (resolution is terminal). An upheld refund rejected by
// the reversal's risk gates — a frozen payee (403), an
// overdraft-protected payee whose balance cannot cover the refund, or
// the reversal cap after a sibling dispute already refunded (409) —
// leaves the dispute open: the resolution never half-commits.
func (s *server) handleResolveDispute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req resolveDisputeRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}

	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dispute id required"})
		return
	}

	d, err := s.ledgerFor(r).ResolveDispute(id, req.Uphold)
	if err != nil {
		if errors.Is(err, ledger.ErrDisputeNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrDisputeAlreadyResolved) || errors.Is(err, ledger.ErrAlreadyReversed) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrDailyLimitExceeded) {
			s.metrics.DailyLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBudgetExceeded) {
			s.metrics.BudgetRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrPeriodClosed) {
			s.metrics.PeriodRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// ledger_disputes_resolved_total counts committed resolutions:
	// upheld and rejected alike — the decision rides the
	// dispute_resolved audit and outbox events.
	s.metrics.DisputesResolvedTotal.Add(1)
	writeJSON(w, http.StatusOK, d)
}

// handleGetDispute implements GET /disputes/{id}: the dispute with its
// current status (open until ResolveDispute moves it to resolved).
// 404 on an unknown dispute ID.
func (s *server) handleGetDispute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dispute id required"})
		return
	}
	d, ok := s.ledgerFor(r).GetDispute(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": ledger.ErrDisputeNotFound.Error()})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type openTermDepositRequest struct {
	ID             string           `json:"id"`
	Account        ledger.AccountID `json:"account"`
	AmountCents    int64            `json:"amount_cents"`
	Currency       string           `json:"currency"`
	TermDays       int64            `json:"term_days"`
	RateBps        int64            `json:"rate_bps"`
	IdempotencyKey string           `json:"idempotency_key"`
	// BusinessDayMaturity opts into business-day settlement (LG-95): a
	// MaturesAt landing on a non-business day moves to the next
	// business day.
	BusinessDayMaturity bool `json:"business_day_maturity"`
}

// handleOpenTermDeposit implements POST /term-deposits: locks
// amount_cents of the account's available funds until term_days from now
// (see ledger.OpenTermDeposit). The principal is off-journal —
// available_cents drops by the locked amount while balance_cents is
// untouched — and interest accrues via POST
// /term-deposits/{id}/accrue. The server generates the deposit ID when
// the client omits it.
//
// A first-time open returns 201; a duplicate idempotency key returns 200
// with the original deposit; malformed requests return 400; an open on a
// frozen or closed account returns 403; an open the account's available
// funds cannot cover returns 422. The currency field is optional and
// defaults to USD; when given it must be a 3-letter uppercase ISO 4217
// code. term_days must be 1..36500 and rate_bps 1..10000.
func (s *server) handleOpenTermDeposit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.metrics.TermDepositsTotal.Add(1)
	var req openTermDepositRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	id := req.ID
	if id == "" {
		id = newID()
	}
	td, duplicate, err := s.ledgerFor(r).OpenTermDeposit(ledger.TermDeposit{
		ID:                id,
		Account:           req.Account,
		AmountCents:       req.AmountCents,
		Currency:          req.Currency,
		TermDays:          req.TermDays,
		RateBps:           req.RateBps,
		IdempotencyKey:    req.IdempotencyKey,
		BusinessDayMaturity: req.BusinessDayMaturity,
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAccountClosed) {
			s.metrics.ClosedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountSuspended) {
			s.metrics.SuspendedRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInsufficientAvailableFunds) {
			s.metrics.TermDepositRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			s.metrics.CurrencyRejections.Add(1)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if duplicate {
		s.metrics.TermDepositIdempotencyHits.Add(1)
		writeJSON(w, http.StatusOK, td)
		return
	}
	writeJSON(w, http.StatusCreated, td)
}

// handleGetTermDeposit implements GET /term-deposits/{id}: returns the
// deposit record. An unknown deposit returns 404.
func (s *server) handleGetTermDeposit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "term deposit id required"})
		return
	}
	td, ok := s.ledgerFor(r).GetTermDeposit(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "term deposit not found"})
		return
	}
	writeJSON(w, http.StatusOK, td)
}

// handleAccrueTermInterest implements POST /term-deposits/{id}/accrue:
// books one day's interest on an active term deposit (see
// ledger.AccrueTermInterest). The day defaults to today (UTC); pass
// ?asof=RFC3339 to book a specific day. The booking is idempotent per
// (deposit, day): re-running a day replays the original row with 200.
//
// A first-time accrual returns 201; an unknown deposit returns 404;
// accruing a non-active deposit or past maturity returns 422; a ledger
// without a configured interest-expense account returns 500 (operator
// misconfiguration — see LEDGER_TD_INTEREST_EXPENSE).
func (s *server) handleAccrueTermInterest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "term deposit id required"})
		return
	}
	s.metrics.TermDepositAccrualsTotal.Add(1)
	var asof time.Time
	if raw := r.URL.Query().Get("asof"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid asof: " + err.Error()})
			return
		}
		asof = t
	}
	entry, duplicate, err := s.ledgerFor(r).AccrueTermInterest(id, asof)
	if err != nil {
		if errors.Is(err, ledger.ErrTermDepositNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrTermDepositNotActive) || errors.Is(err, ledger.ErrTermDepositMatured) {
			s.metrics.TermDepositRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrTermDepositInterestAccountNotConfigured) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, entry)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

// handleMatureTermDeposit implements POST /term-deposits/{id}/mature:
// releases an active deposit's principal back to available funds once
// its maturity date has arrived (see ledger.MatureTermDeposit).
// Maturing is idempotent — an already-matured deposit returns 200 as-is.
// An unknown deposit returns 404; maturing before maturity returns 422.
func (s *server) handleMatureTermDeposit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "term deposit id required"})
		return
	}
	s.metrics.TermDepositMaturitiesTotal.Add(1)
	td, err := s.ledgerFor(r).MatureTermDeposit(id, time.Now())
	if err != nil {
		if errors.Is(err, ledger.ErrTermDepositNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrTermNotMatured) || errors.Is(err, ledger.ErrTermDepositNotActive) {
			s.metrics.TermDepositRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, td)
}

type withdrawTermDepositRequest struct {
	PenaltyBps int64 `json:"penalty_bps"`
}

// handleWithdrawTermDeposit implements POST
// /term-deposits/{id}/withdraw: releases an active deposit's principal
// before maturity, booking the early-withdrawal penalty when
// penalty_bps > 0 (see ledger.WithdrawTermDeposit). Interest already
// journaled is not clawed back. The withdrawal is idempotent —
// re-withdrawing returns the original receipt with 200.
//
// An unknown deposit returns 404; a malformed penalty rate returns 400;
// withdrawing a matured deposit returns 422; a positive penalty without
// a configured fee-revenue account returns 500 (operator
// misconfiguration).
func (s *server) handleWithdrawTermDeposit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "term deposit id required"})
		return
	}
	s.metrics.TermDepositWithdrawalsTotal.Add(1)
	var req withdrawTermDepositRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	receipt, err := s.ledgerFor(r).WithdrawTermDeposit(id, req.PenaltyBps, time.Now())
	if err != nil {
		if errors.Is(err, ledger.ErrTermDepositNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrTermDepositMatured) {
			s.metrics.TermDepositRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidTermDepositPenalty) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrTermDepositPenaltyAccountNotConfigured) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if receipt.Duplicate {
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	writeJSON(w, http.StatusCreated, receipt)
}

// handleSweepTermDeposits implements POST /term-deposits/sweep: the
// operator's daily batch — accrues interest through today on every
// active term deposit and matures every deposit whose maturity has
// arrived (see ledger.SweepTermDeposits). Accrual is best-effort per
// deposit; the deterministic interest IDs make retries idempotent. It
// returns 200 {"matured": N, "accrued": M}.
func (s *server) handleSweepTermDeposits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.metrics.TermDepositSweepsTotal.Add(1)
	matured, accrued := s.ledgerFor(r).SweepTermDeposits(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"matured": matured, "accrued": accrued})
}

type grantCreditLineRequest struct {
	Account    ledger.AccountID `json:"account"`
	LimitCents int64            `json:"limit_cents"`
	Currency   string           `json:"currency"`
	RateBps    int64            `json:"rate_bps"`
}

// handleGrantCreditLine implements POST /credit-lines: grants a credit
// line of limit_cents in the currency to the account (see
// ledger.GrantCreditLine). A protected account with a line may carry a
// negative balance down to -limit; utilization beyond the floor is
// rejected with 422 ErrCreditLimitExceeded. Granting is create-only —
// re-granting an existing line returns 409; change it with POST
// /credit-lines/reduce. Malformed requests return 400. The currency
// field is optional and defaults to USD; rate_bps must be 1..10000.
func (s *server) handleGrantCreditLine(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.metrics.CreditLinesTotal.Add(1)
	var req grantCreditLineRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	cl, err := s.ledgerFor(r).GrantCreditLine(req.Account, req.LimitCents, req.Currency, req.RateBps)
	if err != nil {
		if errors.Is(err, ledger.ErrCreditLineExists) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			s.metrics.CurrencyRejections.Add(1)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, cl)
}

type reduceCreditLineRequest struct {
	Account    ledger.AccountID `json:"account"`
	Currency   string           `json:"currency"`
	LimitCents int64            `json:"limit_cents"`
}

// handleReduceCreditLine implements POST /credit-lines/reduce: lowers a
// credit line's limit (see ledger.ReduceCreditLine). The new limit must
// not sit below the line's current utilization (422); reducing to zero
// revokes the line. An unknown line returns 404; malformed requests
// return 400.
func (s *server) handleReduceCreditLine(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req reduceCreditLineRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	cl, err := s.ledgerFor(r).ReduceCreditLine(req.Account, req.Currency, req.LimitCents)
	if err != nil {
		if errors.Is(err, ledger.ErrCreditLineNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditLineBelowUtilization) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cl)
}

// handleGetCreditLine implements GET /credit-lines?account=&currency=:
// reports a credit line's limit, drawn utilization, and remaining
// availability. An unknown line returns 404; a missing account returns
// 400.
func (s *server) handleGetCreditLine(w http.ResponseWriter, r *http.Request) {
	account := r.URL.Query().Get("account")
	if account == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account query parameter required"})
		return
	}
	currency := r.URL.Query().Get("currency")
	cl, ok := s.ledgerFor(r).GetCreditLine(ledger.AccountID(account), currency)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no credit line for this account and currency"})
		return
	}
	used, available, limit, _ := s.ledgerFor(r).CreditLineUtilization(ledger.AccountID(account), cl.Currency)
	writeJSON(w, http.StatusOK, map[string]any{
		"account":         cl.Account,
		"currency":        cl.Currency,
		"limit_cents":     limit,
		"used_cents":      used,
		"available_cents": available,
		"rate_bps":        cl.RateBps,
		"granted_at":      cl.GrantedAt,
	})
}

// handleAccrueCreditInterest implements POST
// /credit-lines/accrue-interest: books one day's interest on a credit
// line's utilization (see ledger.AccrueCreditInterest). The body carries
// account and an optional currency; the day defaults to today (UTC) and
// can be pinned with ?asof=RFC3339. The booking is idempotent per
// (account, currency, day): re-running a day replays the original row
// with 200.
//
// A first-time accrual returns 201; an unknown line returns 404;
// interest that would breach the line's floor returns 422
// (ErrCreditLimitExceeded); a ledger without a configured
// interest-income account returns 500 (operator misconfiguration — see
// LEDGER_CREDIT_INTEREST_INCOME).
// decodeOptionalJSONBody is decodeJSONBody for endpoints where the
// request body is fully optional (parameters also arrive via the query
// string). A missing body leaves v at its zero value instead of
// rejecting with "invalid JSON body: EOF".
func (s *server) decodeOptionalJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 {
		return true
	}
	return s.decodeJSONBody(w, r, v)
}

func (s *server) handleAccrueCreditInterest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.metrics.CreditInterestAccrualsTotal.Add(1)
	var req struct {
		Account  ledger.AccountID `json:"account"`
		Currency string           `json:"currency"`
	}
	if !s.decodeOptionalJSONBody(w, r, &req) {
		return
	}
	var asof time.Time
	if raw := r.URL.Query().Get("asof"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid asof: " + err.Error()})
			return
		}
		asof = t
	}
	// Account and currency ride in the JSON body or the query string,
	// whichever the client sent.
	account, currency := req.Account, req.Currency
	if account == "" {
		account = ledger.AccountID(r.URL.Query().Get("account"))
	}
	if currency == "" {
		currency = r.URL.Query().Get("currency")
	}
	entry, duplicate, err := s.ledgerFor(r).AccrueCreditInterest(account, currency, asof)
	if err != nil {
		if errors.Is(err, ledger.ErrCreditLineNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditLimitExceeded) {
			s.metrics.CreditLimitRejections.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrCreditInterestIncomeNotConfigured) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, entry)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

type registerECLExposureRequest struct {
	Account       ledger.AccountID `json:"account"`
	Currency      string           `json:"currency"`
	ExposureCents int64            `json:"exposure_cents"`
	DaysPastDue   int64            `json:"days_past_due"`
}

// handleRegisterECLExposure implements POST /ecl/exposures: registers
// (or re-registers) a manual credit exposure for an obligor without a
// credit line (see ledger.RegisterECLExposure). The body carries the
// account, the exposure in cents, and the days past due; the currency
// defaults to USD. Re-registering updates the amount and DPD in place.
// Malformed requests return 400.
func (s *server) handleRegisterECLExposure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.metrics.ECLExposureRequestsTotal.Add(1)
	var req registerECLExposureRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	exp, err := s.ledgerFor(r).RegisterECLExposure(req.Account, req.Currency, req.ExposureCents, req.DaysPastDue)
	if err != nil {
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			s.metrics.CurrencyRejections.Add(1)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, exp)
}

type setECLDaysPastDueRequest struct {
	Account     ledger.AccountID `json:"account"`
	Currency    string           `json:"currency"`
	DaysPastDue int64            `json:"days_past_due"`
}

// handleSetECLDaysPastDue implements POST /ecl/exposures/dpd: updates
// the days-past-due tracking for an (account, currency) exposure (see
// ledger.SetECLDaysPastDue). It works on both exposure sources — a
// manually registered exposure, or a credit-line-sourced exposure,
// whose row is created on first update. With neither a manual exposure
// nor a credit line the call returns 404; malformed requests return
// 400.
func (s *server) handleSetECLDaysPastDue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req setECLDaysPastDueRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	if err := s.ledgerFor(r).SetECLDaysPastDue(req.Account, req.Currency, req.DaysPastDue); err != nil {
		if errors.Is(err, ledger.ErrECLExposureNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			s.metrics.CurrencyRejections.Add(1)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleListECLExposures implements GET /ecl/exposures: lists every
// tracked ECL exposure row, sorted by (account, currency) (see
// ledger.ECLExposures).
func (s *server) handleListECLExposures(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, s.ledgerFor(r).ECLExposures())
}

// handleBookECLProvision implements POST /ecl/provisions: books one
// day's expected credit loss provision for an obligor (see
// ledger.BookECLProvision). The body carries account and an optional
// currency; the day defaults to today (UTC) and can be pinned with
// ?asof=RFC3339. The booking is idempotent per (account, currency,
// day): re-running a day replays the original row with 200.
//
// A first-time provision returns 201; an obligor with no tracked
// exposure returns 404; a ledger without the configured expense or
// allowance account returns 500 (operator misconfiguration — see
// LEDGER_ECL_EXPENSE and LEDGER_ECL_ALLOWANCE); a dust provision
// returns 400.
func (s *server) handleBookECLProvision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.metrics.ECLProvisionRequestsTotal.Add(1)
	var req struct {
		Account  ledger.AccountID `json:"account"`
		Currency string           `json:"currency"`
	}
	if !s.decodeOptionalJSONBody(w, r, &req) {
		return
	}
	var asof time.Time
	if raw := r.URL.Query().Get("asof"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid asof: " + err.Error()})
			return
		}
		asof = t
	}
	// Account and currency ride in the JSON body or the query string,
	// whichever the client sent.
	account, currency := req.Account, req.Currency
	if account == "" {
		account = ledger.AccountID(r.URL.Query().Get("account"))
	}
	if currency == "" {
		currency = r.URL.Query().Get("currency")
	}
	entry, duplicate, err := s.ledgerFor(r).BookECLProvision(account, currency, asof)
	if err != nil {
		s.metrics.ECLProvisionRejectionsTotal.Add(1)
		if errors.Is(err, ledger.ErrECLExposureNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrECLLossExpenseNotConfigured) ||
			errors.Is(err, ledger.ErrECLAllowanceNotConfigured) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if duplicate {
		s.metrics.ECLProvisionReplaysTotal.Add(1)
		writeJSON(w, http.StatusOK, entry)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

// handleSetECLStageConfig implements POST /ecl/config: replaces the
// ledger's IFRS 9 staging configuration (see ledger.SetECLStageConfig).
// An invalid configuration returns 400; the change is structural
// config and never bumps the journal version.
func (s *server) handleSetECLStageConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var cfg ledger.ECLStageConfig
	if !s.decodeJSONBody(w, r, &cfg) {
		return
	}
	if err := s.ledgerFor(r).SetECLStageConfig(cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.ledgerFor(r).ECLStageConfig())
}

// resolveAccountRef resolves an account reference carried by a path or
// query parameter: a registered alias maps to the account it names, and
// anything else is used as the account ID verbatim (see
// ledger.ResolveAccount). The second return value is the account's
// current alias ("" when the account has none), echoed on the
// balance/snapshot/trial-balance responses (LG-31).
//
// Alias resolution applies to the account-scope read endpoints
// (balance, snapshot, trial-balance, balance-at, entries, statement,
// merkle-proof, rollup, ofx, balance-history, and the alias endpoints
// themselves). Write endpoints (parent, tags, close, freeze, alias
// management aside) keep taking canonical account IDs: writes are
// audited against the canonical ID, and resolving aliases on the write
// path would let a rebound alias silently retarget in-flight
// automation. Reads are where human operators paste aliases, so reads
// resolve and writes do not.
func (s *server) resolveAccountRef(r *http.Request, ref string) (ledger.AccountID, string) {
	return s.ledgerFor(r).ResolveAccount(ref)
}

// handleBalance implements GET /accounts/{id}/balance.
//
// available_cents is the account's spendable funds: net balance minus
// active authorization holds (see POST /holds) minus funds frozen by
// pending transfer reviews (see POST /transfers/{id}/approve). A hold
// or a review freeze reserves funds without moving them, so
// balance_cents keeps reporting the journaled net while
// available_cents reports what can still be authorized.
func (s *server) handleBalance(w http.ResponseWriter, r *http.Request) {
	s.metrics.BalanceQueries.Add(1)
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	account, alias := s.resolveAccountRef(r, id)
	l := s.ledgerFor(r)
	resp := map[string]any{
		"account":         string(account),
		"balance_cents":   l.Balance(account),
		"available_cents": l.Available(account),
		"frozen":          l.IsFrozen(account),
	}
	if alias != "" {
		resp["alias"] = alias
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSnapshot implements GET /accounts/{id}/snapshot.
// Returns the account's balance together with the ledger version at read
// time, so reconciliation consumers can tell whether anything changed
// between two reads without comparing full entry logs.
func (s *server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	account, alias := s.resolveAccountRef(r, id)
	l := s.ledgerFor(r)
	balance, version := l.Snapshot(account)
	resp := map[string]any{
		"account":       string(account),
		"balance_cents": balance,
		"version":       version,
		"frozen":        l.IsFrozen(account),
	}
	if alias != "" {
		resp["alias"] = alias
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMerkleProof implements GET /accounts/{id}/merkle-proof[?currency=XXX]
// (LG-78): the inclusion proof for one account's balance in the snapshot
// balance-Merkle tree (see ledger/merkle.go). The proof is self-contained:
// it carries the leaf (the committed balance), the sibling hash path up
// to the root, and the root itself, so an offline verifier needs nothing
// else — ledger.VerifyMerkleProof recomputes the leaf hash and folds the
// siblings, reporting whether they reach the anchored root.
//
// currency is optional and defaults to USD; an unknown account (no leaf
// in the tree) reports 404 — there is no committed balance to prove.
func (s *server) handleMerkleProof(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	account, _ := s.resolveAccountRef(r, id)
	currency := r.URL.Query().Get("currency")
	proof, rootHex, version, err := s.ledgerFor(r).MerkleProofFor(account, currency)
	if err != nil {
		switch {
		case errors.Is(err, ledger.ErrMerkleLeafNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, ledger.ErrInvalidCurrency):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	if currency == "" {
		currency = ledger.DefaultCurrency
	}
	leafHex := hex.EncodeToString(ledger.MerkleLeafEncoding(proof.Leaf))
	siblings := make([]string, len(proof.Siblings))
	for i, sib := range proof.Siblings {
		siblings[i] = hex.EncodeToString(sib[:])
	}
	s.metrics.MerkleProofQueries.Add(1)
	writeJSON(w, http.StatusOK, map[string]any{
		"account":       string(account),
		"currency":      currency,
		"balance_cents": proof.Leaf.BalanceCents,
		"version":       version,
		"merkle_root":   rootHex,
		"leaf_index":    proof.LeafIndex,
		// The hex canonical leaf encoding, so a verifier can recompute
		// the leaf hash without reimplementing the field layout:
		// sha256(0x00 || leaf) must equal the first proof step.
		"leaf":          leafHex,
		"leaf_encoding": "u64be(len(account)) || account || u64be(len(currency)) || currency || i64be(balance_cents), hashed as sha256(0x00 || encoding); inner nodes sha256(0x01 || left || right); odd last node pairs with itself",
		"siblings":      siblings,
	})
}

// handleBalanceAt implements GET /accounts/{id}/balance-at?version=N[&currency=XXX].
// Returns the account's net balance as of ledger version N — the balance
// after exactly N successful posts — for audit replay and point-in-time
// reconciliation. The version /snapshot reports can be fed back here to
// reproduce the balance that was current then.
//
// version is required and must be a non-negative integer; a version beyond
// the current ledger version is 422 (the future has no balance yet).
// currency is optional and defaults to USD. Like /balance and /snapshot,
// an unknown account reports zero.
func (s *server) handleBalanceAt(w http.ResponseWriter, r *http.Request) {
	s.metrics.BalanceAtQueries.Add(1)
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	account, _ := s.resolveAccountRef(r, id)
	version, err := strconv.ParseUint(r.URL.Query().Get("version"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version query parameter is required as a non-negative integer"})
		return
	}
	currency := r.URL.Query().Get("currency")
	if currency == "" {
		currency = ledger.DefaultCurrency
	}
	balance, err := s.ledgerFor(r).BalanceAt(account, currency, version)
	if err != nil {
		if errors.Is(err, ledger.ErrVersionInFuture) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":       string(account),
		"currency":      currency,
		"version":       version,
		"balance_cents": balance,
	})
}

// handleOFXExport implements GET /accounts/{id}/ofx:
// an OFX 2.x bank statement for the account, generated from the journal
// (see ledger.ExportOFX). Query parameters: currency (optional, default
// all currencies as separate statement segments), since/until (optional
// RFC 3339 bounds, half-open [since, until)), after_entry_id (optional
// resume checkpoint from a previous response's X-Ledger-OFX-Checkpoint
// header). The body is the OFX document; the checkpoint of the last
// exported entry rides the response header so clients can page long
// statements without duplicating or skipping rows.
func (s *server) handleOFXExport(w http.ResponseWriter, r *http.Request) {
	s.metrics.OFXExports.Add(1)
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	account, _ := s.resolveAccountRef(r, id)
	q := r.URL.Query()
	var since, until time.Time
	if raw := q.Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be RFC 3339"})
			return
		}
		since = t
	}
	if raw := q.Get("until"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "until must be RFC 3339"})
			return
		}
		until = t
	}
	doc, checkpoint, err := s.ledgerFor(r).ExportOFX(ledger.OFXExportOptions{
		Account:      account,
		Currency:     q.Get("currency"),
		Since:        since,
		Until:        until,
		AfterEntryID: q.Get("after_entry_id"),
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/x-ofx; charset=utf-8")
	if checkpoint != "" {
		w.Header().Set("X-Ledger-OFX-Checkpoint", checkpoint)
	}
	w.WriteHeader(http.StatusOK)
	w.Write(doc)
}

// handlePain001Export implements GET /exports/pain001: an ISO 20022
// pain.001.001.03 customer credit transfer initiation generated from the
// journaled transfer principals (see ledger.ExportPain001). Query
// parameters: from/to (optional RFC 3339 time bounds, half-open
// [from, to), the OFX export convention), debtor/creditor (optional
// payer/payee account filters), currency (optional ISO 4217 code). A
// malformed timestamp, an inverted window, or an invalid currency is a
// 400, fail-fast; a payment whose debtor or creditor has no registered
// IBAN fails the whole export with a 422 (fail-closed, see
// ledger.ErrMissingAccountIBAN — no forged identifiers). The body is the
// pain.001 XML document. The export is read-only: it bumps no version
// and books nothing, but every export emits a pain001_exported audit
// event.
func (s *server) handlePain001Export(w http.ResponseWriter, r *http.Request) {
	s.metrics.Pain001Exports.Add(1)
	q := r.URL.Query()
	var from, to time.Time
	if raw := q.Get("from"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "from must be RFC 3339"})
			return
		}
		from = t
	}
	if raw := q.Get("to"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to must be RFC 3339"})
			return
		}
		to = t
	}
	doc, err := s.ledgerFor(r).ExportPain001(ledger.Pain001ExportOptions{
		Debtor:   ledger.AccountID(q.Get("debtor")),
		Creditor: ledger.AccountID(q.Get("creditor")),
		Currency: q.Get("currency"),
		Since:    from,
		Until:    to,
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ledger.ErrMissingAccountIBAN) {
			status = http.StatusUnprocessableEntity
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(doc)
}

// pain001ReceiptRequest is one bank receipt in the POST
// /exports/pain001/{msgid}/receipts body: the pain.002-style per-tx
// status report. Unknown JSON fields fail fast, like every other POST
// endpoint.
type pain001ReceiptRequest struct {
	TxID   string `json:"tx_id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// pain001ReceiptsRequest is the POST
// /exports/pain001/{msgid}/receipts body: the batch of bank receipts
// for one instruction, applied atomically (see
// ledger.RecordPain001Receipts).
type pain001ReceiptsRequest struct {
	Receipts []pain001ReceiptRequest `json:"receipts"`
}

// handlePain001Status implements GET /exports/pain001/{msgid}/status
// (LG-99): the payment instruction's lifecycle record — msg_id, state,
// exported_at, tx_count, ctrl_sum_cents, currency, and the per-tx
// states. An instruction the ledger never exported is a 404: receipts
// and transitions are keyed by the deterministic export MsgId, so an
// unknown name is a client error, not an empty report.
func (s *server) handlePain001Status(w http.ResponseWriter, r *http.Request) {
	s.metrics.Pain001StatusQueries.Add(1)
	msgID := r.PathValue("msgid")
	if msgID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "msg_id required"})
		return
	}
	rec, ok := s.ledgerFor(r).PainInstruction(msgID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown pain.001 payment instruction"})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handlePain001Receipts implements POST
// /exports/pain001/{msgid}/receipts (LG-99): records a batch of
// pain.002-style bank receipts for the instruction's transfers. The
// batch is applied atomically — one bad receipt (unknown transfer,
// unknown status, illegal transition) refuses the whole batch, so the
// record can never half-absorb a bank file. Replaying a receipt for a
// transfer already in that state is a no-op (idempotent), so
// re-delivered bank files are safe. Error mapping: unknown msg_id is a
// 404; an empty batch is a 400; an unknown tx_id, an unknown status,
// or an illegal transition is a 422 (the request is well-formed but the
// state machine cannot apply it). The operator identity comes from the
// x-ledger-actor header, like the other operator actions — the actor
// lands in the hash-chained pain001_status audit events.
func (s *server) handlePain001Receipts(w http.ResponseWriter, r *http.Request) {
	s.metrics.Pain001Receipts.Add(1)
	msgID := r.PathValue("msgid")
	if msgID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "msg_id required"})
		return
	}
	var req pain001ReceiptsRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	receipts := make([]ledger.Pain001Receipt, 0, len(req.Receipts))
	for _, pr := range req.Receipts {
		receipts = append(receipts, ledger.Pain001Receipt{TxID: pr.TxID, Status: pr.Status, Reason: pr.Reason})
	}
	rec, err := s.ledgerFor(r).RecordPain001Receipts(msgID, receipts, ledger.ActorFromContext(actx))
	if err != nil {
		switch {
		case errors.Is(err, ledger.ErrUnknownPainInstruction):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, ledger.ErrEmptyPain001Receipts):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, ledger.ErrUnknownPain001Tx),
			errors.Is(err, ledger.ErrInvalidPain001ReceiptStatus),
			errors.Is(err, ledger.ErrIllegalPain001Transition):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		default:
			log.Printf("ledger-api-go: POST /exports/pain001/%s/receipts: %v", msgID, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "recording receipts failed"})
		}
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handlePain001Settle implements POST /exports/pain001/{msgid}/settle
// (LG-99): the operator's reconcile confirmation for a payment
// instruction. Every transfer the bank accepted advances to settled;
// unreceipted (exported) transfers stay exported and rejected ones
// stay rejected — the operator cannot mark money settled the bank never
// confirmed. This is deliberately an explicit operator action, not a
// side effect of the automatic POST /reconcile scan: settlement
// confirmation is an attestation, and the reconcile report instead
// discloses unsettled instructions (pain001_pending) so the operator
// knows which ones still await this confirmation. The operator identity
// comes from the x-ledger-actor header.
func (s *server) handlePain001Settle(w http.ResponseWriter, r *http.Request) {
	s.metrics.Pain001Settlements.Add(1)
	msgID := r.PathValue("msgid")
	if msgID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "msg_id required"})
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	rec, err := s.ledgerFor(r).SettlePain001Instruction(msgID, ledger.ActorFromContext(actx))
	if err != nil {
		if errors.Is(err, ledger.ErrUnknownPainInstruction) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("ledger-api-go: POST /exports/pain001/%s/settle: %v", msgID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "settle confirmation failed"})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleStatement implements GET /accounts/{id}/statement: a JSON
// account statement for the half-open window [from, to) (see
// ledger.GenerateStatement). Query parameters: from / to (optional
// RFC 3339 timestamps; omitted from defaults to the first day of the
// current UTC month at 00:00, omitted to defaults to now). A malformed
// timestamp or an inverted window (from after to) is a 400, fail-fast.
// The response carries account, period{from,to}, opening_balances[],
// closing_balances[] (per currency), entries[] (effective journal rows
// in (CreatedAt, ID) order — pending-review rows are journaled but not
// effective, so the statement excludes them like the OFX export does),
// fee_summary[] (per-currency transfer-fee rollup), event_summary
// (freeze transitions, dual-control reviews, overdraft-protection
// state), generated_at, and the ledger version read at. There is no
// special auth, like the /ofx export and the other read-only account
// endpoints. The statement is a pure query: it bumps no version and
// leaves no trace.
func (s *server) handleStatement(w http.ResponseWriter, r *http.Request) {
	s.metrics.StatementQueries.Add(1)
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	account, _ := s.resolveAccountRef(r, id)
	q := r.URL.Query()
	var from, to time.Time
	if raw := q.Get("from"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "from must be RFC 3339"})
			return
		}
		from = t
	}
	if raw := q.Get("to"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to must be RFC 3339"})
			return
		}
		to = t
	}
	stmt, err := s.ledgerFor(r).GenerateStatement(account, from, to)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, stmt)
}

// ofxImportRequest is the POST /ofx/import body: the ledger account the
// statement belongs to and the raw OFX document.
type ofxImportRequest struct {
	Account ledger.AccountID `json:"account"`
	OFX     string           `json:"ofx"`
}

// handleOFXImport implements POST /ofx/import: it parses an OFX 2.x bank
// statement and returns a pre-posting (draft) import report (see
// ledger.ImportOFX). Nothing is posted, no version is bumped: the
// response tells the operator what the statement claims and what the
// ledger already has. Re-importing the same document returns the
// original report with duplicate=true. A missing account, an empty
// document, a malformed document, or a statement naming a different
// account is a 400. There is no special auth, like the /ofx export
// endpoint.
func (s *server) handleOFXImport(w http.ResponseWriter, r *http.Request) {
	s.metrics.OFXImports.Add(1)
	var req ofxImportRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	if req.Account == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account is required"})
		return
	}
	if strings.TrimSpace(req.OFX) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ofx document is required"})
		return
	}
	report, err := s.ledgerFor(r).ImportOFX(req.Account, []byte(req.OFX), ledger.OFXImportOptions{})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// camt053ImportRequest is the POST /camt053/import body: the ledger
// account the statement belongs to, and the raw camt.053 XML document.
type camt053ImportRequest struct {
	Account ledger.AccountID `json:"account"`
	CAMT053 string           `json:"camt053"`
}

// handleCAMT053Import implements POST /camt053/import: it parses an
// ISO 20022 camt.053 bank statement and returns a pre-posting (draft)
// import report (see ledger.ImportCAMT053). Nothing is posted, no
// version is bumped: the response tells the operator what the
// statement claims and what the ledger already has. Re-importing the
// same document returns the original report with duplicate=true. A
// missing account, an empty document, a malformed document, or a
// statement naming a different account is a 400. There is no special
// auth, like the /ofx/import endpoint.
func (s *server) handleCAMT053Import(w http.ResponseWriter, r *http.Request) {
	s.metrics.CAMT053Imports.Add(1)
	var req camt053ImportRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	if req.Account == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account is required"})
		return
	}
	if strings.TrimSpace(req.CAMT053) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "camt053 document is required"})
		return
	}
	report, err := s.ledgerFor(r).ImportCAMT053(req.Account, []byte(req.CAMT053), ledger.CAMT053ImportOptions{})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleBalanceHistory implements GET /balance-history: the balance time
// series the background recorder samples (see ledger/balance_history.go),
// for reconciliation dashboards and fintech reporting. Query parameters:
// account (required), currency (default USD), since/until (optional
// RFC 3339 bounds). With no history directory configured the endpoint
// answers 404 fail-closed rather than inventing an empty series.
func (s *server) handleBalanceHistory(w http.ResponseWriter, r *http.Request) {
	if s.balanceHistoryDir == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "balance history not configured"})
		return
	}
	q := r.URL.Query()
	account := q.Get("account")
	if account == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account query parameter is required"})
		return
	}
	acct, _ := s.resolveAccountRef(r, account)
	currency := q.Get("currency")
	if currency == "" {
		currency = ledger.DefaultCurrency
	}
	var since, until time.Time
	if raw := q.Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be RFC 3339"})
			return
		}
		since = t
	}
	if raw := q.Get("until"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "until must be RFC 3339"})
			return
		}
		until = t
	}
	points, err := ledger.QueryBalanceHistory(s.balanceHistoryDir, acct, currency, since, until)
	if err != nil {
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read balance history"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":  account,
		"currency": currency,
		"points":   points,
	})
}

// handleSnapshotDiff implements GET /snapshots/diff?from=&to= (LG-70):
// the three-dimension diff (balances / entries / frozen state) between
// two backup snapshot versions of the same ledger. Both versions must
// resolve from the backup directory — a version no backup chain can
// construct is a 404, and an unconfigured backup directory is a 404
// fail-closed. The diff is read-only: it loads fresh in-memory ledgers
// from the snapshot files and writes nothing to the live ledger, the
// audit log, or the backup directory.
func (s *server) handleSnapshotDiff(w http.ResponseWriter, r *http.Request) {
	if s.snapshotBackupDir == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "snapshot backups not configured"})
		return
	}
	q := r.URL.Query()
	from, err := parseSnapshotVersionParam(q.Get("from"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `from must be a non-negative integer ledger version`})
		return
	}
	to, err := parseSnapshotVersionParam(q.Get("to"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `to must be a non-negative integer ledger version`})
		return
	}
	diff, err := ledger.DiffSnapshotVersionsWithKeys(s.snapshotBackupDir, from, to, s.backupKeys)
	if err != nil {
		if errors.Is(err, ledger.ErrSnapshotVersionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrBackupUnknownKeyID) || errors.Is(err, ledger.ErrBackupDecryptFailed) || errors.Is(err, ledger.ErrBackupEnvelopeInvalid) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load snapshots for diff"})
		return
	}
	s.metrics.SnapshotDiffsTotal.Add(1)
	writeJSON(w, http.StatusOK, diff)
}

// parseSnapshotVersionParam parses a ?from=/​?to= snapshot version: a
// non-empty decimal integer that fits in a uint64.
func parseSnapshotVersionParam(raw string) (uint64, error) {
	if raw == "" {
		return 0, errors.New("missing")
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// handleTrialBalance implements GET /accounts/{id}/trial-balance.
// Returns the account's double-entry breakdown — total debits, total
// credits, and net balance — at the current ledger version. Net balance
// always equals total debits minus total credits; the endpoint is the
// read-side of the accounting equation for reconciliation tooling.
// Like /balance and /snapshot, an unknown account reports zeros.
func (s *server) handleTrialBalance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	account, _ := s.resolveAccountRef(r, id)
	writeJSON(w, http.StatusOK, s.ledgerFor(r).TrialBalance(account))
}

// setParentRequest is the body of POST /accounts/{id}/parent. An empty
// parent clears the assignment.
type setParentRequest struct {
	Parent ledger.AccountID `json:"parent"`
}

// denylistAddRequest is the body of POST /denylist: exactly one of
// account (exact account ID) or prefix (prefix pattern) must be set.
type denylistAddRequest struct {
	Account string `json:"account"`
	Prefix  string `json:"prefix"`
}

// denylistImportRequest is the body of POST /denylist/import: a batch of
// denylist rows for loading a sanctions feed at once.
type denylistImportRequest struct {
	Entries []denylistAddRequest `json:"entries"`
}

// handleAddDenylist implements POST /denylist: the operator-facing
// sanctions-list stop. The body carries exactly one of {"account": "<id>"}
// (exact account ID) or {"prefix": "<prefix>"} (prefix pattern matching
// every account ID it starts). Adding is idempotent. The operator
// identity comes from the x-ledger-actor header (see actorCtx) and is
// recorded on the audit event and the entry itself.
func (s *server) handleAddDenylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req denylistAddRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	if (req.Account == "") == (req.Prefix == "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must set exactly one of account or prefix"})
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	if req.Account != "" {
		entry := s.ledgerFor(r).AddDeniedAccountCtx(actx, ledger.AccountID(req.Account))
		writeJSON(w, http.StatusCreated, entry)
		return
	}
	entry, err := s.ledgerFor(r).AddDeniedPrefixCtx(actx, req.Prefix)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

// handleListDenylist implements GET /denylist: lists every sanctions-list
// row, exact entries first then prefix patterns, each sorted by value.
func (s *server) handleListDenylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	entries := s.ledgerFor(r).Denylist()
	if entries == nil {
		entries = []ledger.DenylistEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// handleRemoveDenylist implements DELETE /denylist/{value}: removes one
// sanctions-list row. The value names an exact account ID by default; with
// ?prefix=true it names a prefix pattern. Removing a non-listed row is
// 404. The operator identity comes from the x-ledger-actor header.
func (s *server) handleRemoveDenylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	value := r.PathValue("value")
	if value == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "denylist value required"})
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	isPrefix := r.URL.Query().Get("prefix") == "true"
	var removed bool
	if isPrefix {
		removed = s.ledgerFor(r).RemoveDeniedPrefixCtx(actx, value)
	} else {
		removed = s.ledgerFor(r).RemoveDeniedAccountCtx(actx, ledger.AccountID(value))
	}
	if !removed {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "denylist entry not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": value, "prefix": isPrefix, "removed": true})
}

// handleImportDenylist implements POST /denylist/import: installs a batch
// of sanctions-list rows at once — the operator shape for loading a
// sanctions feed. The import is atomic: a malformed row aborts the whole
// import with 400 before anything is installed. Rows already listed are
// skipped, not duplicated. The response reports added/skipped counts.
func (s *server) handleImportDenylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req denylistImportRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	specs := make([]ledger.DenylistSpec, 0, len(req.Entries))
	for _, e := range req.Entries {
		specs = append(specs, ledger.DenylistSpec{Account: e.Account, Prefix: e.Prefix})
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	added, skipped, err := s.ledgerFor(r).ImportDenylistCtx(actx, specs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "skipped": skipped})
}

// handleSetParent implements POST /accounts/{id}/parent: links an account
// into the sub-account hierarchy (see ledger.SetParent), the structure
// GET /accounts/{id}/rollup aggregates over. The body names the parent;
// sending {"parent":""} clears the link. Like Freeze, linking is
// structural and does not bump the ledger version. A self-assignment is
// 400; an assignment that would close a cycle is 422.
func (s *server) handleSetParent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req setParentRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return
	}

	if err := s.ledgerFor(r).SetParent(ledger.AccountID(id), req.Parent); err != nil {
		if errors.Is(err, ledger.ErrParentCycle) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrDailyLimitExceeded) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	parent, _ := s.ledgerFor(r).Parent(ledger.AccountID(id))
	writeJSON(w, http.StatusOK, map[string]any{"account": id, "parent": string(parent)})
}

// handleRollup implements GET /accounts/{id}/rollup: the balance rollup of
// the sub-account subtree rooted at the account — the account's own
// balances plus every descendant's, per currency (see ledger.Rollup).
// Like /balance, an unknown account rolls up to just itself with zero
// balances.
func (s *server) handleRollup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	account, _ := s.resolveAccountRef(r, id)
	writeJSON(w, http.StatusOK, s.ledgerFor(r).Rollup(account))
}

type setAccountTagsRequest struct {
	Tags map[string]string `json:"tags"`
}

// handleSetAccountTags implements PUT /accounts/{id}/tags: replaces the
// account's tag set (see ledger.SetAccountTags), the classification
// labels used by the tag-dimension rollup (GET /rollup?tag=k:v). The body
// is {"tags":{"k":"v",...}}; an empty or missing map clears the account's
// tags. Tagging is structural and does not bump the ledger version. An
// invalid key/value (outside [A-Za-z0-9._-] or over 64 characters) is
// 400.
func (s *server) handleSetAccountTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req setAccountTagsRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return
	}

	l := s.ledgerFor(r)
	if err := l.SetAccountTags(ledger.AccountID(id), req.Tags); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	tags, _ := l.AccountTagsOf(ledger.AccountID(id))
	writeJSON(w, http.StatusOK, map[string]any{"account": id, "tags": tags})
}

// setAliasRequest is the body of PUT /accounts/{id}/alias: the alias to
// bind to the account. An empty or missing alias clears the account's
// alias (see ledger.ClearAlias).
type setAliasRequest struct {
	Alias string `json:"alias"`
}

// handleSetAlias implements PUT /accounts/{id}/alias: binds a
// human-readable alias to the account (see ledger.SetAlias). The alias
// must be 1-64 characters of [A-Za-z0-9._-]; an empty or missing alias
// clears the account's alias. Binding an alias that already names a
// different account is 409 (ledger.ErrAliasConflict); a malformed alias
// is 400. {id} is an account reference, so an existing alias can be
// rebound through the account it names. Alias changes are structural —
// they do not bump the ledger version — and are audited as
// alias_set/alias_changed/alias_removed.
func (s *server) handleSetAlias(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req setAliasRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return
	}

	l := s.ledgerFor(r)
	account, _ := s.resolveAccountRef(r, id)
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	if err := l.SetAliasCtx(actx, account, req.Alias); err != nil {
		switch {
		case errors.Is(err, ledger.ErrAliasConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	s.metrics.AliasSetsTotal.Add(1)
	alias, _ := l.AliasOf(account)
	writeJSON(w, http.StatusOK, map[string]any{"account": string(account), "alias": alias})
}

// handleGetTransferAllowlist implements GET /accounts/{id}/allowlist:
// returns the payer's transfer beneficiary allowlist (see
// ledger.TransferAllowlistOf). A payer with no list installed has the
// gate disabled: the response carries allowlist_enabled false and an
// empty beneficiaries array.
func (s *server) handleGetTransferAllowlist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	beneficiaries, enabled := s.ledgerFor(r).TransferAllowlistOf(ledger.AccountID(id))
	if beneficiaries == nil {
		beneficiaries = []ledger.AccountID{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":           id,
		"allowlist_enabled": enabled,
		"beneficiaries":     beneficiaries,
	})
}

type setTransferAllowlistRequest struct {
	// Beneficiaries is the payer's allowed payee set: while installed,
	// POST /transfers from the payer only books transfers whose
	// to_account is in this set (see ledger.SetTransferAllowlist). An
	// empty or missing set disables the gate — it is equivalent to
	// clearing the list.
	Beneficiaries []ledger.AccountID `json:"beneficiaries"`
}

// handleSetTransferAllowlist implements PUT /accounts/{id}/allowlist:
// installs (or replaces) the payer's transfer beneficiary allowlist
// (see ledger.SetTransferAllowlist). An empty beneficiaries set
// disables the gate. The body is strict JSON — unknown fields are 400.
// Structural config: no journal version bump. An empty beneficiary
// entry is 400.
func (s *server) handleSetTransferAllowlist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	defer r.Body.Close()

	// Same transport contract as the other /accounts/{id} config
	// endpoints: bounded body, strict decoding, no trailing garbage.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req setTransferAllowlistRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return
	}

	l := s.ledgerFor(r)
	if err := l.SetTransferAllowlist(ledger.AccountID(id), req.Beneficiaries); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	beneficiaries, enabled := l.TransferAllowlistOf(ledger.AccountID(id))
	if beneficiaries == nil {
		beneficiaries = []ledger.AccountID{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":           id,
		"allowlist_enabled": enabled,
		"beneficiaries":     beneficiaries,
	})
}

// handleGetAlias implements GET /accounts/{id}/alias: returns the
// account's current alias (see ledger.AliasOf). {id} is an account
// reference, so the alias can be looked up by the account ID or by the
// alias itself. An account with no alias is 404
// (ledger.ErrAliasNotSet).
func (s *server) handleGetAlias(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	account, _ := s.resolveAccountRef(r, id)
	alias, ok := s.ledgerFor(r).AliasOf(account)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": ledger.ErrAliasNotSet.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": string(account), "alias": alias})
}

// setExternalMappingRequest is the body of POST
// /accounts/{id}/external-mappings: the external system the account is
// known to (see ledger.SetExternalMapping), the identifier it carries
// there, and an optional operator note.
type setExternalMappingRequest struct {
	System     string `json:"system"`
	ExternalID string `json:"external_id"`
	Note       string `json:"note"`
}

// handleSetExternalMapping implements POST /accounts/{id}/external-mappings:
// declares (or updates) one external account mapping for the account
// (see ledger.SetExternalMapping) — the explicit link between the
// internal account and the identifier an external system knows it by
// (a core-banking account number, a PSP merchant number). The body is
// strict JSON: {"system":"bank","external_id":"...","note":"..."}.
// Unknown fields are 400, and so are an empty/invalid system, an empty
// or overlong external ID, or an overlong note. The declaration is
// structural — it does not bump the ledger version — and is audited as
// external_mapping_set. {id} is the canonical account ID: writes never
// resolve aliases, so a rebound alias cannot silently retarget an
// audited mapping (see resolveAccountRef).
func (s *server) handleSetExternalMapping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	var req setExternalMappingRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	account := ledger.AccountID(id)
	if err := s.ledgerFor(r).SetExternalMappingCtx(actx, account, req.System, req.ExternalID, req.Note); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":     string(account),
		"system":      req.System,
		"external_id": req.ExternalID,
		"note":        req.Note,
	})
}

// handleListExternalMappings implements GET /accounts/{id}/external-mappings:
// lists every external mapping declared for the account (see
// ledger.ListExternalMappings), sorted by (system, external_id). An
// account with no declared mappings returns an empty list, like the
// account reads that report zero rows. The mappings are the identifier
// links the file exports (OFX statements, payment instructions)
// resolve external account identifiers from.
func (s *server) handleListExternalMappings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	account := ledger.AccountID(id)
	mappings := s.ledgerFor(r).ListExternalMappings(account)
	writeJSON(w, http.StatusOK, map[string]any{
		"account":  string(account),
		"mappings": mappings,
	})
}

// handleRemoveExternalMapping implements DELETE
// /accounts/{id}/external-mappings: removes one declared mapping (see
// ledger.RemoveExternalMapping). The row is named by the ?system= and
// ?external_id= query parameters. Removing an undeclared row is 404;
// a removed row is audited as external_mapping_removed. {id} is the
// canonical account ID, like the POST endpoint above.
func (s *server) handleRemoveExternalMapping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	system := r.URL.Query().Get("system")
	externalID := r.URL.Query().Get("external_id")
	if system == "" || externalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query parameters system and external_id are required"})
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	account := ledger.AccountID(id)
	if !s.ledgerFor(r).RemoveExternalMappingCtx(actx, account, system, externalID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "external mapping not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":     string(account),
		"system":      system,
		"external_id": externalID,
		"removed":     true,
	})
}

type setSweepReserveRequest struct {
	Account    ledger.AccountID `json:"account"`
	Currency   string           `json:"currency"`
	FloorCents int64            `json:"floor_cents"`
}

// handleSetSweepReserve implements PUT /sweep-reserves: configures the
// sweep reserve floor for one account in one currency (see
// ledger.SetSweepReserve). PostSweep moves only the balance in excess of
// the floor to the target; the floor stays behind as the account's
// operating float. The floor must be positive (clear via DELETE); a
// non-positive floor or a malformed currency is 400
// (ledger.ErrInvalidSweepReserve). The body is strict JSON — unknown
// fields fail fast, like every other write endpoint. Reserve changes
// are structural: they do not bump the ledger version and are audited
// as sweep_reserve_set/sweep_reserve_changed.
func (s *server) handleSetSweepReserve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req setSweepReserveRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	if req.Account == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account required"})
		return
	}

	account, _ := s.resolveAccountRef(r, string(req.Account))
	if err := s.ledgerFor(r).SetSweepReserve(account, req.Currency, req.FloorCents); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	floor, _ := s.ledgerFor(r).SweepReserve(account, req.Currency)
	writeJSON(w, http.StatusOK, map[string]any{
		"account":     string(account),
		"currency":    normalizedReserveCurrency(req.Currency),
		"floor_cents": floor,
	})
}

// handleListSweepReserves implements GET /sweep-reserves: lists the
// configured sweep reserve floors (see ledger.SetSweepReserve), sorted
// by (account, currency). Optional ?account= and ?currency= filter the
// list. An empty list means no reserves are configured.
func (s *server) handleListSweepReserves(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	q := r.URL.Query()
	wantAccount := q.Get("account")
	wantCurrency := q.Get("currency")

	out := make([]ledger.SweepReserve, 0)
	for _, row := range s.ledgerFor(r).SweepReserves() {
		if wantAccount != "" && string(row.Account) != wantAccount {
			continue
		}
		if wantCurrency != "" && row.Currency != wantCurrency {
			continue
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sweep_reserves": out})
}

// handleClearSweepReserve implements DELETE /sweep-reserves: removes the
// sweep reserve floor for one account in one currency (see
// ledger.ClearSweepReserve). ?account= is required; ?currency=
// defaults to the ledger's default currency. Clearing a reserve that
// was never set is a silent no-op, returning 200.
func (s *server) handleClearSweepReserve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	q := r.URL.Query()
	account := q.Get("account")
	if account == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account query parameter required"})
		return
	}
	if err := s.ledgerFor(r).ClearSweepReserve(ledger.AccountID(account), q.Get("currency")); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// normalizedReserveCurrency echoes the effective currency of a sweep
// reserve: the request's currency when given, the ledger default
// otherwise. SetSweepReserve already normalized it — this is only the
// response echo, kept in one place.
func normalizedReserveCurrency(currency string) string {
	if currency == "" {
		return ledger.DefaultCurrency
	}
	return currency
}

// handleTagRollup implements GET /rollup?tag=k:v: the balance rollup of
// every account carrying the (key, value) tag pair, per currency (see
// ledger.TagRollupByTag) — the tag dimension across the whole ledger,
// without requiring a hierarchy link between the accounts. A pair that
// matches no account returns an empty account list with zero balances.
// The parameter is "k:v": a missing or malformed tag parameter, or a key
// or value outside the tag charset, is 400.
func (s *server) handleTagRollup(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("tag")
	key, value, found := strings.Cut(raw, ":")
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tag query must be \"k:v\""})
		return
	}
	if err := ledger.CheckTagSegment(key); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid tag key: " + err.Error()})
		return
	}
	if err := ledger.CheckTagSegment(value); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid tag value: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.ledgerFor(r).TagRollupByTag(key, value))
}

// handleVerifyEntries implements GET /entries/verify. It recomputes the
// ledger's tamper-evident audit chain and reports whether it is intact:
// 200 {"ok":true,"links":N,"head":"<hex>","anchor":{...}} on success. The
// anchor section is the external-anchoring status (LG-44): whether a key
// is configured, how many checkpoints exist, the latest checkpoint, and
// whether the checkpoint history verifies against the live chain. A
// broken chain is an operator-level integrity incident, not a client
// error, so verification failure returns 500 {"ok":false,"error":"..."}.
func (s *server) handleVerifyEntries(w http.ResponseWriter, r *http.Request) {
	s.metrics.VerifyRequests.Add(1)
	if err := s.ledgerFor(r).VerifyChain(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	head, links := s.ledgerFor(r).ChainHead()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"links":  links,
		"head":   head,
		"anchor": s.ledgerFor(r).AnchorStatus(),
		"tsa":    s.ledgerFor(r).TimestampAnchorStatus(),
	})
}

// handleAnchorCheckpoint implements POST /entries/anchor: signs the
// current audit-chain head with the configured Ed25519 key and appends
// the checkpoint to checkpoints.jsonl (see ledger.Anchor). 200 returns
// the checkpoint. Without a configured anchor key or audit directory the
// server cannot anchor — a deployment misconfiguration, so 500, not 404:
// there is nothing for the caller to fix in the request.
func (s *server) handleAnchorCheckpoint(w http.ResponseWriter, r *http.Request) {
	cp, err := s.ledgerFor(r).Anchor()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.metrics.AnchorsTotal.Add(1)
	writeJSON(w, http.StatusOK, cp)
}

// handleTSAAnchor implements POST /entries/tsa-anchor: countersigns the
// current audit-chain head with the configured RFC 3161 timestamp
// authority and appends the token to tsa_anchors.jsonl (see
// ledger.AnchorTimestamp). 200 returns the anchor. Without a configured
// TSA URL or audit directory the server cannot anchor — a deployment
// misconfiguration, so 500, not 404. A TSA-side failure (network,
// rejection) is 502: the TSA is an upstream dependency, the periodic
// worker retries on its next tick, and posting never waits for an
// anchor.
func (s *server) handleTSAAnchor(w http.ResponseWriter, r *http.Request) {
	tsaURL := s.ledgerFor(r).TSAURL()
	if tsaURL == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "TSA anchoring not configured (LEDGER_TSA_URL)"})
		return
	}
	anchor, err := s.ledgerFor(r).AnchorTimestamp(r.Context(), &ledger.RFC3161Client{URL: tsaURL})
	if err != nil {
		if errors.Is(err, ledger.ErrTSAURLNotConfigured) || errors.Is(err, ledger.ErrTSAAuditDirRequired) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		} else {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return
	}
	s.metrics.TSAAnchorsTotal.Add(1)
	writeJSON(w, http.StatusOK, anchor)
}

// handleAuditVerify implements GET /audit/verify: the operator-facing
// integrity check for the structured audit log's hash chain (LG-32). It
// replays every audit file — the current file plus history in time
// order, including .gz archives — recomputing each entry's SHA-256 seal
// and reporting the first break with its exact position:
//
//	200 {"ok":true,"checked_entries":128,"skipped_lines":0,
//	     "files_checked":3,"head":"9f2c…","first_break":null}
//	200 {"ok":false,...,"first_break":{"file":"audit-2026-10-09.jsonl",
//	     "line":42,"entry_id":"tr-7","expected_prev":"ab…",
//	     "actual_prev":"cd…","reason":"prev_mismatch"},...}
//
// Like POST /reconcile, an unhealthy finding is still a successful
// request: the findings live in the body, so the status is always 200.
// Corrupt lines are skipped and disclosed as skipped_lines — a skip is
// not a break. Without LEDGER_AUDIT_DIR there is nothing to verify, so
// the endpoint 404s fail-closed.
func (s *server) handleAuditVerify(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.ledgerFor(r).AuditDir()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "audit log disabled"})
		return
	}
	s.metrics.AuditVerifyTotal.Add(1)
	rep, err := ledger.VerifyAuditLog(dir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if !rep.Ok {
		s.metrics.AuditVerifyBreaks.Add(1)
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleAuditExport implements GET /audit/export (LG-128): the
// operator-facing read-only export of the sealed audit chain. It copies
// the audit files for the ?from=&to= UTC-day range (inclusive,
// "2006-01-02"; omitted means all) into a temp dir as a read-only
// snapshot, then streams the snapshot's manifest.json back as the
// response body:
//
//	200 {"format":"audit-chain-export/1","exported_at":"…",
//	     "files":[{"name":"audit-2026-10-09.jsonl","day":"2026-10-09",
//	               "sha256":"…","size":1234}],
//	     "report":{"ok":true,"checked_entries":128,"skipped_lines":0,
//	               "files_checked":3,"head":"9f2c…"},
//	     "signed":true,"signature":{"kid":"k1","mac":"…"}}
//
// The temp dir is removed after the response: the endpoint is stateless,
// and the manifest (per-file SHA-256, seal-chain head, verification
// report) is the compliance evidence. Operators who need a persistent
// snapshot call ledger.ExportAuditChain directly (a small Go program is
// enough; see the README). The manifest is HMAC-signed with
// LEDGER_AUDIT_SIGNING_KEYS when configured, and honestly records
// signed:false when not.
//
// Like GET /audit/verify, an unhealthy finding is still a 200 — the
// manifest's report carries ok:false with the first break implied by
// VerifyAuditLog. Bad day formats are 400. Without LEDGER_AUDIT_DIR the
// endpoint 404s fail-closed.
func (s *server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.ledgerFor(r).AuditDir()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "audit log disabled"})
		return
	}
	q := r.URL.Query()
	var keys []ledger.AuditSigningKey
	if raw := os.Getenv("LEDGER_AUDIT_SIGNING_KEYS"); raw != "" {
		ks, err := ledger.ParseAuditSigningKeys(raw)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "invalid LEDGER_AUDIT_SIGNING_KEYS: " + err.Error()})
			return
		}
		keys = ks
	}
	tmp, err := os.MkdirTemp("", "ledger-audit-export-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "export temp dir: " + err.Error()})
		return
	}
	defer os.RemoveAll(tmp)
	manifest, err := ledger.ExportAuditChain(dir, ledger.AuditChainExportOptions{
		FromDay:     q.Get("from"),
		ToDay:       q.Get("to"),
		ExportDir:   tmp,
		SigningKeys: keys,
	})
	if err != nil {
		if ledger.IsAuditExportInputError(err) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.metrics.AuditExportTotal.Add(1)
	if !manifest.Report.Ok {
		s.metrics.AuditExportBreaks.Add(1)
	}
	// Stream the exact manifest.json bytes the snapshot holds, so a
	// downstream verifier can re-hash and re-verify the MAC against the
	// same bytes.
	raw, err := os.ReadFile(filepath.Join(tmp, "manifest.json"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// handleOutboxEvents implements GET /events: the operator-facing query
// API over the domain-event outbox (LG-53).
//
//		GET /events?since=<rfc3339>&until=<rfc3339>&type=<t1,t2>&account=<id>&limit=100&cursor=<opaque>
//
//	  - since/until bound the event time (RFC3339, optional; until is
//	    exclusive).
//	  - type filters to one or more comma-separated event types
//	    (entry_posted, transfer_posted, hold_created, ...); omitted means
//	    all types.
//	  - account matches the event's account or either leg of an
//	    entry_posted event.
//	  - limit caps the page at 1..1000 (default 100).
//	  - cursor is the opaque page cursor (base64url of the last seen seq);
//	    the response's next_cursor resumes after it, and is empty on the
//	    last page.
//
// Without LEDGER_OUTBOX_DIR the endpoint 404s fail-closed: there is no
// stream to query, and the 404 says so explicitly.
func (s *server) handleOutboxEvents(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.ledgerFor(r).OutboxDir()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "domain event outbox disabled (LEDGER_OUTBOX_DIR unset)"})
		return
	}
	q := r.URL.Query()
	since, until, limit, err := parseEntryListQuery(q)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var types []string
	if v := firstQuery(q, "type"); v != "" {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				types = append(types, t)
			}
		}
	}
	afterSeq, err := ledger.DecodeOutboxCursor(firstQuery(q, "cursor"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	events, next, corrupt, err := ledger.ReadOutboxEvents(dir, ledger.OutboxQuery{
		Since:    since,
		Until:    until,
		Types:    types,
		Account:  ledger.AccountID(firstQuery(q, "account")),
		AfterSeq: afterSeq,
		Limit:    limit,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":        events,
		"next_cursor":   next,
		"corrupt_lines": corrupt,
	})
}

// handleWebhookDeliveries implements GET /webhooks/deliveries (LG-102):
// the operator-facing query over recent outbound webhook delivery
// attempts, latest first.
//
//	GET /webhooks/deliveries?endpoint=<name>&status=<pending|delivered|failed>&limit=50
//
//   - endpoint filters to one registered endpoint name.
//   - status filters to pending, delivered, or failed (anything else
//     is 400).
//   - limit caps the page at 1..100 (default 50; anything else is 400).
//
// Without LEDGER_WEBHOOKS the endpoint 404s fail-closed, like
// handleOutboxEvents without LEDGER_OUTBOX_DIR: there is no delivery
// log to query, and the 404 says so explicitly.
func (s *server) handleWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	d := s.webhooks
	if d == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "outbound webhooks disabled (LEDGER_WEBHOOKS unset)"})
		return
	}
	q := r.URL.Query()
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid limit: want 1..100"})
			return
		}
		limit = n
	}
	status := q.Get("status")
	switch status {
	case "", ledger.WebhookDeliveryPending, ledger.WebhookDeliveryDelivered, ledger.WebhookDeliveryFailed:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid status: want pending, delivered, or failed"})
		return
	}
	recs := d.Deliveries(ledger.WebhookDeliveryQuery{
		Endpoint: q.Get("endpoint"),
		Status:   status,
		Limit:    limit,
	})
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": recs})
}

// freezeAccountRequest is the optional body of POST
// /accounts/{id}/freeze: an RFC3339 timestamp at which the freeze lifts
// automatically (see ledger.FreezeUntil). Omitted (or an empty body)
// means a manual freeze, exactly like before timed freezes existed.
type freezeAccountRequest struct {
	Until *time.Time `json:"until,omitempty"`
}

// handleFreezeAccount implements POST /accounts/{id}/freeze: the
// operator-facing risk-control stop. A frozen account rejects every new
// POST /entries that names it as either leg with 403, while balance,
// snapshot, trial-balance, entries, chain verification, and the
// end-of-day reconcile keep working. Freezing is idempotent and does not
// bump the ledger version (balances are unchanged by a freeze).
//
// The optional {"until": "<RFC3339>"} body schedules the freeze to lift
// automatically at that time (a timed freeze); without it the freeze is
// manual. A non-future timestamp is rejected with 400.
// actorCtx builds the per-request audit-attribution context (LG-73)
// from the x-ledger-actor header: the operator identity the audit event
// records for freeze/approve/reject/close operator actions, defaulting
// to "system" when the header is absent. An overlong value or one
// containing control characters is rejected with a 400 — the actor lands
// in the hash-chained audit trail, so it must stay a sane single line.
func (s *server) actorCtx(w http.ResponseWriter, r *http.Request) (context.Context, bool) {
	actor := ledger.NormalizeAuditActor(r.Header.Get("x-ledger-actor"))
	if len(actor) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "x-ledger-actor too long (max 128)"})
		return nil, false
	}
	for i := 0; i < len(actor); i++ {
		if c := actor[i]; c < 0x20 || c == 0x7f {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "x-ledger-actor must not contain control characters"})
			return nil, false
		}
	}
	return ledger.ContextWithActor(r.Context(), actor), true
}

func (s *server) handleFreezeAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	var req freezeAccountRequest
	if r.ContentLength != 0 {
		if !s.decodeJSONBody(w, r, &req) {
			return
		}
	}
	until := time.Time{}
	if req.Until != nil {
		until = *req.Until
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	if err := s.ledgerFor(r).FreezeUntilCtx(actx, ledger.AccountID(id), until); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resp := map[string]any{"account": id, "frozen": true}
	if deadline, ok := s.ledgerFor(r).FreezeAutoUnfreezeAt(ledger.AccountID(id)); ok {
		resp["auto_unfreeze_at"] = deadline.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUnfreezeAccount implements POST /accounts/{id}/unfreeze: lifts a
// freeze applied with POST /accounts/{id}/freeze. Unfreezing an account
// that was never frozen is a no-op.
func (s *server) handleUnfreezeAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	s.ledgerFor(r).UnfreezeCtx(actx, ledger.AccountID(id))
	writeJSON(w, http.StatusOK, map[string]any{"account": id, "frozen": false})
}

// handleSuspendAccount implements POST /accounts/{id}/suspend: the
// operator-facing operational pause. A suspended account rejects every
// new journal-writing request that names it as either leg with 403,
// while balance, snapshot, trial-balance, entries, chain verification,
// and the end-of-day reconcile keep working. Suspending is idempotent
// and does not bump the ledger version (balances are unchanged by a
// suspension).
func (s *server) handleSuspendAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	suspension, alreadySuspended, err := s.ledgerFor(r).SuspendAccountCtx(actx, ledger.AccountID(id))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !alreadySuspended {
		s.metrics.AccountSuspendsTotal.Add(1)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":           id,
		"suspended":         true,
		"already_suspended": alreadySuspended,
		"suspended_at":      suspension.SuspendedAt.UTC().Format(time.RFC3339),
	})
}

// handleResumeAccount implements POST /accounts/{id}/resume: lifts a
// suspension applied with POST /accounts/{id}/suspend. Resuming an
// account that is not suspended is a no-op.
func (s *server) handleResumeAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	resumed := s.ledgerFor(r).ResumeAccountCtx(actx, ledger.AccountID(id))
	writeJSON(w, http.StatusOK, map[string]any{"account": id, "suspended": false, "resumed": resumed})
}

// registerVirtualAccountRequest is the POST /virtual-accounts body.
type registerVirtualAccountRequest struct {
	VAN    ledger.AccountID `json:"van"`
	Parent ledger.AccountID `json:"parent"`
}

// handleRegisterVirtualAccount implements POST /virtual-accounts:
// registers (or re-registers) a van -> parent mapping (see
// ledger.RegisterVirtualAccount). Strict JSON, size-limited body (see
// decodeJSONBody). A validation failure is a 400; re-registering an
// identical mapping is an idempotent no-op. The response carries the
// mapping and the mapping version counter.
func (s *server) handleRegisterVirtualAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req registerVirtualAccountRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	l := s.ledgerFor(r)
	if err := l.RegisterVirtualAccount(req.VAN, req.Parent); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	parent, _ := l.VirtualAccountParent(req.VAN)
	writeJSON(w, http.StatusOK, map[string]any{
		"van":     string(req.VAN),
		"parent":  string(parent),
		"version": l.VirtualAccountVersion(),
	})
}

// handleListVirtualAccounts implements GET /virtual-accounts: every
// van -> parent mapping, sorted by VAN, plus the mapping version
// counter.
func (s *server) handleListVirtualAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	l := s.ledgerFor(r)
	mappings := l.ListVirtualAccounts()
	rows := make([]map[string]string, 0, len(mappings))
	for _, m := range mappings {
		rows = append(rows, map[string]string{"van": string(m.VAN), "parent": string(m.Parent)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"virtual_accounts": rows,
		"version":          l.VirtualAccountVersion(),
	})
}

// handleUnregisterVirtualAccount implements DELETE
// /virtual-accounts/{van}: removes the mapping (see
// ledger.UnregisterVirtualAccount). Unregistering an unknown VAN is an
// idempotent no-op reporting deleted=false, like unfreezing a
// never-frozen account.
func (s *server) handleUnregisterVirtualAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	van := r.PathValue("van")
	if van == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "van required"})
		return
	}
	l := s.ledgerFor(r)
	_, had := l.VirtualAccountParent(ledger.AccountID(van))
	if err := l.UnregisterVirtualAccount(ledger.AccountID(van)); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"van":     van,
		"deleted": had,
		"version": l.VirtualAccountVersion(),
	})
}

// handleCloseAccount implements POST /accounts/{id}/close: the
// terminal account lifecycle state (see ledger.CloseAccount). A closed
// account rejects every new POST /entries, POST /transfers, hold,
// sweep, merge, reversal, batch, and closing entry through it with 403,
// while balance, snapshot, trial-balance, entries, chain verification,
// and the end-of-day reconcile keep working. Closing is irreversible —
// there is no unclose — and idempotent: re-closing returns the original
// closure record (with "already_closed": true) instead of failing.
//
// The optional {"cascade": true} body closes the account's whole
// sub-account subtree (see ledger.SetParent) in the same call, the
// merchant-offboarding path.
func (s *server) handleCloseAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	var req struct {
		Cascade bool `json:"cascade"`
	}
	if r.ContentLength != 0 {
		if !s.decodeJSONBody(w, r, &req) {
			return
		}
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	closure, alreadyClosed, err := s.ledgerFor(r).CloseAccountCtx(actx, ledger.AccountID(id), req.Cascade)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !alreadyClosed {
		s.metrics.AccountClosesTotal.Add(1)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":        id,
		"closed":         true,
		"already_closed": alreadyClosed,
		"closed_at":      closure.ClosedAt.UTC().Format(time.RFC3339),
		"cascade":        req.Cascade,
	})
}

// setJournalRuleRequest is the body of PUT /rules/{id}: the rule fields.
// The rule ID comes from the path.
type setJournalRuleRequest struct {
	Enabled            bool             `json:"enabled"`
	Description        string           `json:"description,omitempty"`
	MatchDebitAccount  ledger.AccountID `json:"match_debit_account,omitempty"`
	MatchCreditAccount ledger.AccountID `json:"match_credit_account,omitempty"`
	MatchCurrency      string           `json:"match_currency,omitempty"`
	MinAmountCents     int64            `json:"min_amount_cents,omitempty"`
	MaxAmountCents     int64            `json:"max_amount_cents,omitempty"`
	Legs               []ledger.RuleLeg `json:"legs"`
}

// handleSetJournalRule implements PUT /rules/{id}: creates or replaces an
// opt-in automatic journaling rule (see ledger.SetJournalRule). An
// illegal rule fails 400 with the validation error and stores nothing;
// rule changes are recorded in the structured audit log (op "rule_set").
func (s *server) handleSetJournalRule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()

	s.metrics.JournalRuleSetsTotal.Add(1)

	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "rule id required"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req setJournalRuleRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return
	}

	rule := ledger.JournalRule{
		ID:                 id,
		Enabled:            req.Enabled,
		Description:        req.Description,
		MatchDebitAccount:  req.MatchDebitAccount,
		MatchCreditAccount: req.MatchCreditAccount,
		MatchCurrency:      req.MatchCurrency,
		MinAmountCents:     req.MinAmountCents,
		MaxAmountCents:     req.MaxAmountCents,
		Legs:               req.Legs,
	}
	if err := s.ledgerFor(r).SetJournalRule(rule); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	stored, _ := s.ledgerFor(r).GetJournalRule(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"rule":         stored,
		"rule_version": s.ledgerFor(r).JournalRuleVersion(),
	})
}

// handleDeleteJournalRule implements DELETE /rules/{id}: removes an
// automatic journaling rule. Deleting a rule that was never set is a
// no-op reporting deleted=false. The change is recorded in the structured
// audit log (op "rule_delete").
func (s *server) handleDeleteJournalRule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()

	s.metrics.JournalRuleDeletesTotal.Add(1)

	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "rule id required"})
		return
	}
	deleted := s.ledgerFor(r).DeleteJournalRule(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":      deleted,
		"rule_version": s.ledgerFor(r).JournalRuleVersion(),
	})
}

// handleListJournalRules implements GET /rules: lists the installed
// automatic journaling rules, sorted by rule ID, with the rule-set
// mutation counter.
func (s *server) handleListJournalRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rules":        s.ledgerFor(r).JournalRules(),
		"rule_version": s.ledgerFor(r).JournalRuleVersion(),
	})
}

// handleClosePeriod implements POST /periods/{id}/close: locks the
// accounting period id ("YYYY-MM", UTC month). Journal entries whose
// timestamp falls in a closed period are rejected with 422
// (ErrPeriodClosed) by every journal-writing operation, so a closed
// month's books cannot change — the compliance boundary for backdated
// postings. Closing is idempotent; a malformed period ID fails 400.
//
// With ?closing=true the close also books the period's closing entries
// (see ledger.ClosePeriodWithClosing): configured P&L accounts are
// zeroed into the retained-earnings account, dated at the next month's
// first instant. Without closing accounts configured the request fails
// 400; a gate failure (frozen account, overdraft, daily limit, next
// month closed) fails 422/403 and leaves the period open.
func (s *server) handleClosePeriod(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "period id required"})
		return
	}
	s.metrics.PeriodClosesTotal.Add(1)
	requirePreflightClean := r.URL.Query().Get("preflight_clean") == "true"
	if r.URL.Query().Get("closing") == "true" {
		if requirePreflightClean {
			// The closing path has no ledger-level gate variant: run
			// the scan here and refuse before booking anything. Unlike
			// the plain path (see ClosePeriodWithPreflight) the
			// refusal is not audit-logged — the 422 body carries the
			// blocker codes, and the operator can re-run the scan via
			// POST /periods/{id}/preflight.
			rep, perr := s.ledgerFor(r).ClosePeriodPreflight(time.Now().UTC(), id)
			if perr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": perr.Error()})
				return
			}
			if len(rep.Blockers) > 0 {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error":    ledger.ErrPeriodPreflightBlocked.Error(),
					"blockers": rep.Blockers,
				})
				return
			}
		}
		receipt, err := s.ledgerFor(r).ClosePeriodWithClosing(id)
		if err != nil {
			switch {
			case errors.Is(err, ledger.ErrClosingNotConfigured),
				errors.Is(err, ledger.ErrInvalidPeriodID),
				errors.Is(err, ledger.ErrClosingRetainedInPLAccounts),
				errors.Is(err, ledger.ErrClosingEmptyAccount):
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			case errors.Is(err, ledger.ErrClosingIDConflict):
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			case errors.Is(err, ledger.ErrAccountClosed):
				s.metrics.ClosedRejections.Add(1)
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			case errors.Is(err, ledger.ErrAccountFrozen):
				s.metrics.FrozenRejections.Add(1)
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			case errors.Is(err, ledger.ErrAccountSuspended):
				s.metrics.SuspendedRejections.Add(1)
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			case errors.Is(err, ledger.ErrAccountOverdraft):
				s.metrics.OverdraftRejections.Add(1)
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			case errors.Is(err, ledger.ErrCreditLimitExceeded):
				s.metrics.CreditLimitRejections.Add(1)
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			case errors.Is(err, ledger.ErrDailyLimitExceeded):
				s.metrics.DailyLimitRejections.Add(1)
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			case errors.Is(err, ledger.ErrBudgetExceeded):
				s.metrics.BudgetRejections.Add(1)
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			case errors.Is(err, ledger.ErrPeriodClosed):
				s.metrics.PeriodRejections.Add(1)
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			default:
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
			return
		}
		s.metrics.ClosingEntriesTotal.Add(uint64(len(receipt.Entries)))
		writeJSON(w, http.StatusOK, map[string]any{
			"period":         receipt.Period,
			"closed":         receipt.Closed,
			"closing":        true,
			"already_closed": receipt.AlreadyClosed,
			"entries":        receipt.Entries,
		})
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	if err := s.ledgerFor(r).ClosePeriodWithPreflight(actx, id, requirePreflightClean); err != nil {
		if errors.Is(err, ledger.ErrPeriodPreflightBlocked) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"period": id, "closed": true})
}

// handlePeriodPreflight implements POST /periods/{id}/preflight: the
// read-only close-readiness check (see ledger.ClosePeriodPreflight). It
// reports the blockers and warnings that would make a period close
// unclean, without changing anything: no version bump, no journal rows,
// no audit events. A malformed period ID fails 400.
func (s *server) handlePeriodPreflight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "period id required"})
		return
	}
	s.metrics.PeriodPreflightRunsTotal.Add(1)
	rep, err := s.ledgerFor(r).ClosePeriodPreflight(time.Now().UTC(), id)
	if err != nil {
		if errors.Is(err, ledger.ErrInvalidPeriodID) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleReopenPeriod implements POST /periods/{id}/reopen: unlocks a
// period previously closed with POST /periods/{id}/close, so postings
// dated in that month are accepted again. Reopening a period that was
// never closed is a no-op.
func (s *server) handleReopenPeriod(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "period id required"})
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	if err := s.ledgerFor(r).ReopenPeriodCtx(actx, id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"period": id, "closed": false})
}

// closingAccountsRequest is the body of PUT /closing-accounts: the opt-in
// P&L set and retained-earnings account for period-closing entries (see
// ledger.SetClosingAccounts). An empty retained_earnings with no
// pl_accounts clears the configuration.
type closingAccountsRequest struct {
	PLAccounts       []string `json:"pl_accounts"`
	RetainedEarnings string   `json:"retained_earnings"`
}

// handleSetClosingAccounts implements PUT /closing-accounts: declares
// which accounts are zeroed into retained earnings when a period closes
// with ?closing=true. Structural config: no version bump, survives
// snapshots, recorded in the audit chain.
func (s *server) handleSetClosingAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req closingAccountsRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
		return
	}

	pl := make([]ledger.AccountID, 0, len(req.PLAccounts))
	for _, a := range req.PLAccounts {
		pl = append(pl, ledger.AccountID(a))
	}
	if err := s.ledgerFor(r).SetClosingAccounts(pl, ledger.AccountID(req.RetainedEarnings)); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	plOut, retained := s.ledgerFor(r).ClosingAccounts()
	names := make([]string, 0, len(plOut))
	for _, a := range plOut {
		names = append(names, string(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"pl_accounts": names, "retained_earnings": string(retained)})
}

// handleGetClosingAccounts implements GET /closing-accounts: returns the
// configured period-closing account mapping.
func (s *server) handleGetClosingAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	pl, retained := s.ledgerFor(r).ClosingAccounts()
	names := make([]string, 0, len(pl))
	for _, a := range pl {
		names = append(names, string(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"pl_accounts": names, "retained_earnings": string(retained)})
}

// reconcileRequest is the optional POST /reconcile body. base_currency
// opts into the base-currency summary: the report's per-currency totals
// and discrepancies converted through the FX rate table, with the rate
// snapshot, the missing_rates list, and fx_applied=true. An empty body
// (or an empty base_currency) keeps the legacy report with
// fx_applied=false. The code must be a 3-letter uppercase ISO 4217 code.
type reconcileRequest struct {
	BaseCurrency string `json:"base_currency"`
}

// reconcileBodyError is a body-parsing failure with its HTTP status.
type reconcileBodyError struct {
	status int
	msg    string
}

// parseReconcileBody decodes the optional POST /reconcile[/export] body.
// Shared by both endpoints: the body is bounded, strictly decoded, and an
// empty body means "no options" — the legacy scan — so existing cron jobs
// keep working unchanged.
func parseReconcileBody(r *http.Request) (ledger.ReconcileOptions, *reconcileBodyError) {
	opts := ledger.ReconcileOptions{}
	var req reconcileRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	switch err := dec.Decode(&req); {
	case err == io.EOF:
		// No body: legacy report, fx_applied=false.
		return opts, nil
	case err != nil:
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return opts, &reconcileBodyError{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return opts, &reconcileBodyError{http.StatusBadRequest, "invalid JSON body: " + err.Error()}
	default:
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			return opts, &reconcileBodyError{http.StatusBadRequest, "invalid JSON body: unexpected trailing data"}
		}
		opts.BaseCurrency = req.BaseCurrency
		return opts, nil
	}
}

// handleReconcile implements POST /reconcile, the operator-facing end-of-day
// reconciliation job. It runs a full read-only scan of the live ledger — the
// accounting equation, per-account trial balances, idempotency-key health,
// and audit-chain integrity — and returns the report as the response body.
// A reconcile run that reports an unhealthy ledger is still a successful
// request, so the status is always 200: the findings live inside the
// report. The indented JSON body pipes straight into a dated archive:
//
//	curl -s -X POST localhost:8080/reconcile | tee reconcile-$(date +%F).json
//
// With {"base_currency":"USD"} in the request body, the report additionally
// carries a base-currency summary of every totals row and discrepancy,
// converted at the FX rates in effect at scan time:
//
//	curl -s -X POST localhost:8080/reconcile \
//	  -d '{"base_currency":"USD"}' | tee reconcile-$(date +%F).json
func (s *server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	s.metrics.ReconcileRuns.Add(1)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()

	// Bound the body like every other POST endpoint and decode strictly:
	// unknown fields fail fast.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	opts, berr := parseReconcileBody(r)
	if berr != nil {
		writeJSON(w, berr.status, map[string]string{"error": berr.msg})
		return
	}

	report, err := s.ledgerFor(r).ReconcileWithOptions(time.Now(), opts)
	if err != nil {
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("ledger-api-go: POST /reconcile: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "reconcile failed"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := report.WriteJSON(w); err != nil {
		log.Printf("ledger-api-go: POST /reconcile encode error: %v", err)
	}
}

// handleReconcileExport implements POST /reconcile/export (LG-46): the
// same end-of-day scan as POST /reconcile, returned as a signed package
// {report, signature, key_id} — the deterministic report plus the anchor
// key's Ed25519 signature over its canonical bytes, for offline archival
// and third-party verification. The request body is the same optional
// {"base_currency": "..."} as POST /reconcile.
//
// Without a configured anchor key (LEDGER_ANCHOR_KEY) the server cannot
// sign — a deployment misconfiguration, so 500, not 404: there is nothing
// for the caller to fix in the request.
//
//	curl -s -X POST localhost:8080/reconcile/export | tee reconcile-$(date +%F).signed.json
func (s *server) handleReconcileExport(w http.ResponseWriter, r *http.Request) {
	s.metrics.ReconcileExportRuns.Add(1)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()

	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	opts, berr := parseReconcileBody(r)
	if berr != nil {
		writeJSON(w, berr.status, map[string]string{"error": berr.msg})
		return
	}

	exp, err := s.ledgerFor(r).ExportSignedReconcile(time.Now(), opts)
	if err != nil {
		if errors.Is(err, ledger.ErrAnchorKeyNotConfigured) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("ledger-api-go: POST /reconcile/export: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "reconcile export failed"})
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

// largeTxReportRequest is the body of POST /compliance/large-tx-reports.
// Day is optional: empty means today (UTC).
type largeTxReportRequest struct {
	Day string `json:"day"`
}

// handleLargeTxReports implements POST /compliance/large-tx-reports, the
// operator-facing large-transaction (CTR) report generator. It scans the
// journal for one UTC calendar day and generates a report for every
// (account, currency) whose inflow or outflow reaches the configured
// per-currency threshold (see ledger.GenerateLargeTxReports). Generation
// is read-only — it books nothing and never blocks postings — and is
// idempotent: regenerating a day replays the stored reports instead of
// double-reporting into the audit chain.
//
//	curl -s -X POST localhost:8080/compliance/large-tx-reports \
//	  -d '{"day":"2026-10-15"}'
func (s *server) handleLargeTxReports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()

	// Bound the body like every other POST endpoint and decode strictly:
	// unknown fields fail fast.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	var req largeTxReportRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	switch err := dec.Decode(&req); {
	case err == io.EOF:
		// No body: generate for today.
	case err != nil:
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	default:
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: unexpected trailing data"})
			return
		}
	}

	reports, err := s.ledgerFor(r).GenerateLargeTxReports(req.Day)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var generated int
	for _, rep := range reports {
		if !rep.Duplicate {
			generated++
		}
	}
	// Only new reports count: replays must not inflate the compliance
	// counter.
	s.metrics.LargeTxReportsTotal.Add(uint64(generated))
	day := req.Day
	if day == "" {
		day = time.Now().UTC().Format("2006-01-02")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"day":        day,
		"generated":  generated,
		"duplicates": len(reports) - generated,
		"reports":    reports,
	})
}

// parseSettlementExpected parses one ?expected= query value of
// GET /settlement: "<account>:<currency>:<cents>". The account may not be
// empty; cents is a signed integer (negative expectations are legal for
// accounts that net out negative). The currency code itself is validated
// by the ledger when the report runs.
func parseSettlementExpected(raw string) (ledger.SettlementExpectationKey, int64, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 3 || parts[0] == "" {
		return ledger.SettlementExpectationKey{}, 0,
			errors.New("invalid expected value " + strconv.Quote(raw) + `: want "account:currency:cents"`)
	}
	cents, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return ledger.SettlementExpectationKey{}, 0,
			errors.New("invalid expected value " + strconv.Quote(raw) + ": cents must be an integer")
	}
	return ledger.SettlementExpectationKey{Account: ledger.AccountID(parts[0]), Currency: parts[1]}, cents, nil
}

// parseSettlementTolerance parses one ?tolerance= query value of
// GET /settlement: "<account>:<currency>:<absCents>:<bps>". Bounds are
// checked by the ledger when the report runs (abs >= 0, 0 <= bps <=
// 10000); here only the shape and the integers are validated.
func parseSettlementTolerance(raw string) (ledger.SettlementExpectationKey, ledger.SettlementTolerance, error) {
	var zero ledger.SettlementTolerance
	parts := strings.Split(raw, ":")
	if len(parts) != 4 || parts[0] == "" {
		return ledger.SettlementExpectationKey{}, zero,
			errors.New("invalid tolerance value " + strconv.Quote(raw) + `: want "account:currency:absCents:bps"`)
	}
	abs, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return ledger.SettlementExpectationKey{}, zero,
			errors.New("invalid tolerance value " + strconv.Quote(raw) + ": absCents must be an integer")
	}
	bps, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return ledger.SettlementExpectationKey{}, zero,
			errors.New("invalid tolerance value " + strconv.Quote(raw) + ": bps must be an integer")
	}
	key := ledger.SettlementExpectationKey{Account: ledger.AccountID(parts[0]), Currency: parts[1]}
	return key, ledger.SettlementTolerance{AbsCents: abs, Bps: bps}, nil
}

// handleSettlement implements GET /settlement, the operator-facing
// end-of-day settlement view (LG-43): per-(merchant account, UTC day,
// currency) debit/credit/net aggregation with the configured channel
// dimension, optional expected-net reconciliation alerts, and export
// formats for the fintech settlement workflow.
//
// Query parameters (all optional):
//
//	day        "YYYY-MM-DD" UTC calendar day; default today (UTC).
//	expected   repeatable "<account>:<currency>:<cents>": the day-net the
//	           payment provider's settlement file says the account should
//	           have; checked against the ledger's actual net.
//	tolerance  repeatable "<account>:<currency>:<absCents>:<bps>": the
//	           opt-in alert threshold for that expectation. Expectations
//	           without a tolerance row alert on any nonzero difference.
//	format     json (default), csv (the cells view), or sql (SQL text
//	           dump for sqlite3 import — see ledger.SettlementReport.WriteSQL).
//
// Like POST /reconcile, an unhealthy finding (mismatch alerts) is still
// a successful 200: the report carries the alerts, and the HTTP layer
// does not turn reconciliation findings into errors.
func (s *server) handleSettlement(w http.ResponseWriter, r *http.Request) {
	s.metrics.SettlementRuns.Add(1)
	q := r.URL.Query()

	format := q.Get("format")
	if format == "" {
		format = "json"
	}
	switch format {
	case "json", "csv", "sql":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `format must be one of "json", "csv", "sql"`})
		return
	}

	opts := ledger.SettlementOptions{Day: q.Get("day")}
	for _, raw := range q["expected"] {
		key, want, err := parseSettlementExpected(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if opts.ExpectedNets == nil {
			opts.ExpectedNets = map[ledger.SettlementExpectationKey]int64{}
		}
		opts.ExpectedNets[key] = want
	}
	for _, raw := range q["tolerance"] {
		key, tol, err := parseSettlementTolerance(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if opts.Tolerances == nil {
			opts.Tolerances = map[ledger.SettlementExpectationKey]ledger.SettlementTolerance{}
		}
		opts.Tolerances[key] = tol
	}

	report, err := s.ledgerFor(r).Settlement(time.Now(), opts)
	if err != nil {
		if errors.Is(err, ledger.ErrInvalidSettlementDay) ||
			errors.Is(err, ledger.ErrInvalidCurrency) ||
			errors.Is(err, ledger.ErrInvalidSettlementTolerance) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("ledger-api-go: GET /settlement: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "settlement failed"})
		return
	}
	s.metrics.SettlementAlerts.Add(uint64(len(report.Mismatches)))

	switch format {
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=settlement-"+report.Day+".csv")
		w.WriteHeader(http.StatusOK)
		if err := report.WriteCSV(w); err != nil {
			log.Printf("ledger-api-go: GET /settlement csv encode error: %v", err)
		}
	case "sql":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=settlement-"+report.Day+".sql")
		w.WriteHeader(http.StatusOK)
		if err := report.WriteSQL(w); err != nil {
			log.Printf("ledger-api-go: GET /settlement sql encode error: %v", err)
		}
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := report.WriteJSON(w); err != nil {
			log.Printf("ledger-api-go: GET /settlement encode error: %v", err)
		}
	}
}

// handleSettlementNextWindow implements GET /settlement/next-window
// (LG-101): the operator's "when does this currency settle next" query.
//
// Query parameters:
//
//	currency   required: the ISO 4217 code whose settlement window to
//	           query. The currency must have a configured settlement
//	           cutoff.
//
// The response is the next settlement window (RFC3339, in the cutoff's
// timezone), the cutoff it closes, and whether it is today's still-open
// window. A currency with no configured cutoff is 404: the caller asked
// about a window that does not exist.
func (s *server) handleSettlementNextWindow(w http.ResponseWriter, r *http.Request) {
	s.metrics.SettlementNextWindowQueries.Add(1)
	currency := r.URL.Query().Get("currency")
	if currency == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "currency query parameter required"})
		return
	}
	win, err := s.ledgerFor(r).NextSettlementWindow(currency, time.Now())
	if err != nil {
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrNoSettlementCutoff) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("ledger-api-go: GET /settlement/next-window: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "next settlement window failed"})
		return
	}
	writeJSON(w, http.StatusOK, win)
}

// handleListSettlementDeferrals implements GET /settlement/deferred
// (LG-101): lists the transfers deferred to settlement windows, sorted
// by (window, transfer ID). The ?status= query parameter filters to one
// of scheduled, pending, or settled; absent, it lists everything,
// including settled history.
func (s *server) handleListSettlementDeferrals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var statuses []ledger.SettlementDeferralStatus
	if raw := r.URL.Query().Get("status"); raw != "" {
		status := ledger.SettlementDeferralStatus(raw)
		switch status {
		case ledger.DeferralScheduled, ledger.DeferralPending, ledger.DeferralSettled:
			statuses = append(statuses, status)
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `status must be one of "scheduled", "pending", "settled"`})
			return
		}
	}
	deferrals := s.ledgerFor(r).DeferredSettlements(statuses...)
	if deferrals == nil {
		deferrals = []ledger.SettlementDeferral{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deferrals": deferrals})
}

// settlementAdvanceRequest is the body of POST /settlement/advance: the
// optional RFC3339 instant the window-open tick is evaluated against.
// Empty means now.
type settlementAdvanceRequest struct {
	Now string `json:"now"`
}

// handleAdvanceSettlementWindows implements POST /settlement/advance
// (LG-101): the operator's window-open tick. Every scheduled deferral
// whose window has opened moves to pending — the transfer is now in the
// current settlement run. The operation is idempotent: deferrals whose
// window is still in the future, or which already advanced, are
// untouched. The operator identity comes from the x-ledger-actor
// header, like the other operator actions.
func (s *server) handleAdvanceSettlementWindows(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req settlementAdvanceRequest
	if r.ContentLength != 0 {
		if !s.decodeJSONBody(w, r, &req) {
			return
		}
	}
	now := time.Now()
	if req.Now != "" {
		var err error
		now, err = time.Parse(time.RFC3339, req.Now)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "now must be RFC3339"})
			return
		}
	}
	advanced := s.ledgerFor(r).AdvanceSettlementWindows(now)
	if advanced == nil {
		advanced = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"advanced":       advanced,
		"advanced_count": len(advanced),
		"evaluated_at":   now.Format(time.RFC3339),
	})
}

// handleSettleDeferredTransfer implements
// POST /settlement/deferred/{id}/settle (LG-101): the operator's
// confirmation that the settlement run processed the transfer. Only a
// pending deferral settles — a scheduled one must be advanced first
// (see POST /settlement/advance). The operator identity comes from the
// x-ledger-actor header and is recorded on the audit event.
func (s *server) handleSettleDeferredTransfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "deferral transfer id required"})
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	if err := s.ledgerFor(r).SettleDeferredTransfer(id, ledger.ActorFromContext(actx)); err != nil {
		if errors.Is(err, ledger.ErrDeferralNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrDeferralBadTransition) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("ledger-api-go: POST /settlement/deferred settle: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "settle deferred transfer failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"transfer_id": id, "status": string(ledger.DeferralSettled)})
}

// handleAging implements GET /aging, the operator-facing AR aging view
// (LG-50): per-(account, currency) outstanding receivables decomposed by
// age bucket (0-30 / 31-60 / 61-90 / 90+ days), FIFO-attributed from each
// entry's business timestamp, with opt-in overdue alerts.
//
// Query parameters (all optional):
//
//	account            exact account filter; forces the row to appear even
//	                   when the account's balance is not positive.
//	asof               RFC3339 business date the ages are measured against;
//	                   default now.
//	alert_overdue_cents  flag every reported (account, currency) whose
//	                   61-90 + 90+ day overdue cents exceed it; each hit
//	                   emits an aging_alert audit event and bumps
//	                   ledger_aging_alerts_total. Zero or negative
//	                   disables alerting.
//
// Like GET /settlement, an alert finding is still a successful 200: the
// report carries the alerts, and the HTTP layer does not turn findings
// into errors.
func (s *server) handleAging(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var asof time.Time
	if raw := q.Get("asof"); raw != "" {
		var err error
		asof, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "asof must be RFC3339, e.g. 2026-10-10T00:00:00Z"})
			return
		}
	}

	var threshold int64
	if raw := q.Get("alert_overdue_cents"); raw != "" {
		var err error
		threshold, err = strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "alert_overdue_cents must be an integer"})
			return
		}
	}

	report := s.ledgerFor(r).Aging(time.Now(), ledger.AgingOptions{
		AsOf:                       asof,
		Account:                    ledger.AccountID(q.Get("account")),
		OverdueAlertThresholdCents: threshold,
	})
	s.metrics.AgingAlertsTotal.Add(uint64(len(report.Alerts)))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := report.WriteJSON(w); err != nil {
		log.Printf("ledger-api-go: GET /aging encode error: %v", err)
	}
}

// handleIntradayLiquidity serves the read-only intraday liquidity
// forecast (see ledger.IntradayLiquidity) for one UTC day:
// GET /liquidity/intraday?date=YYYY-MM-DD. An absent date defaults to
// today. A malformed date is a 400.
func (s *server) handleIntradayLiquidity(w http.ResponseWriter, r *http.Request) {
	dateStr := r.URL.Query().Get("date")
	date := time.Now()
	if dateStr != "" {
		d, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `date must be "YYYY-MM-DD"`})
			return
		}
		date = d
	}
	writeJSON(w, http.StatusOK, s.ledgerFor(r).IntradayLiquidity(date))
}

func newRouter(l *ledger.Ledger) http.Handler {
	return newServer(l).handler()
}

// newServer builds the server the same way newRouter does but also hands
// the caller the *server, so main can attach background workers (the hold
// sweeper) to its metrics. Tests keep using newRouter. The ledger is
// adopted as the store's default tenant, and any extra options are
// captured so lazily-provisioned tenant ledgers are built with the same
// configuration the default ledger started with.
func newServer(l *ledger.Ledger, opts ...ledger.Option) *server {
	store := ledger.NewTenantStore(opts...)
	store.Adopt(ledger.DefaultTenantID, l)
	return &server{ledger: l, tenants: store, metrics: &Metrics{}, maxBodyBytes: maxRequestBodyBytes()}
}

// ledgerFor resolves the request to its tenant's ledger: the
// /tenants/{id} path prefix (set by the tenant middleware) wins over the
// x-tenant header; a request naming no tenant gets the default tenant's
// ledger, which keeps single-tenant deployments working exactly as
// before.
func (s *server) ledgerFor(r *http.Request) *ledger.Ledger {
	if id, ok := tenantFromContext(r.Context()); ok {
		return s.tenants.For(id)
	}
	return s.ledger
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /entries", s.handleCreateEntry)
	mux.HandleFunc("POST /entries/batch", s.handleCreateBatch)
	mux.HandleFunc("POST /entries/dry-run", s.handleDryRunEntry)
	mux.HandleFunc("POST /transfers", s.handleCreateTransfer)
	mux.HandleFunc("POST /transfers/dry-run", s.handleDryRunTransfer)
	mux.HandleFunc("POST /transfers/reviews/expire", s.handleExpireReviews)
	mux.HandleFunc("POST /transfers/{id}/approve", s.handleApproveTransferReview)
	mux.HandleFunc("POST /transfers/{id}/reject", s.handleRejectTransferReview)
	mux.HandleFunc("GET /transfers/{id}/fee-split", s.handleGetTransferFeeSplit)
	mux.HandleFunc("POST /transfer-schedules", s.handleCreateTransferSchedule)
	mux.HandleFunc("GET /transfer-schedules/{id}", s.handleGetTransferSchedule)
	mux.HandleFunc("GET /transfer-schedules/{id}/runs", s.handleListScheduleRuns)
	mux.HandleFunc("GET /transfer-schedules/{id}/preview", s.handlePreviewTransferSchedule)
	mux.HandleFunc("POST /transfer-schedules/{id}/pause", func(w http.ResponseWriter, r *http.Request) {
		s.scheduleTransition(w, r, "pause")
	})
	mux.HandleFunc("POST /transfer-schedules/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		s.scheduleTransition(w, r, "resume")
	})
	mux.HandleFunc("POST /transfer-schedules/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.scheduleTransition(w, r, "cancel")
	})
	mux.HandleFunc("POST /sweeps", s.handleCreateSweep)
	mux.HandleFunc("POST /sweeps/dry-run", s.handleDryRunSweep)
	mux.HandleFunc("POST /merges", s.handleCreateMerge)
	mux.HandleFunc("POST /entries/{id}/reverse", s.handleReverseEntry)
	mux.HandleFunc("POST /entries/{id}/reverse-partial", s.handleReverseEntryPartial)
	mux.HandleFunc("GET /entries/{id}/links", s.handleEntryLinks)
	// LG-125: direct-debit mandates — create, debit, per-mandate
	// reconciliation query, lifecycle, and the expiry sweep.
	mux.HandleFunc("POST /mandates", s.handleCreateMandate)
	mux.HandleFunc("POST /mandates/expire", s.handleExpireMandates)
	mux.HandleFunc("GET /mandates/{id}", s.handleGetMandate)
	mux.HandleFunc("POST /mandates/{id}/debits", s.handleDebitMandate)
	mux.HandleFunc("GET /mandates/{id}/debits", s.handleListMandateDebits)
	mux.HandleFunc("POST /mandates/{id}/suspend", s.handleSuspendMandate)
	mux.HandleFunc("POST /mandates/{id}/cancel", s.handleCancelMandate)
	mux.HandleFunc("POST /revaluations", s.handleCreateRevaluation)
	mux.HandleFunc("POST /reconcile", s.handleReconcile)
	mux.HandleFunc("POST /reconcile/export", s.handleReconcileExport)
	mux.HandleFunc("POST /compliance/large-tx-reports", s.handleLargeTxReports)
	mux.HandleFunc("GET /settlement", s.handleSettlement)
	mux.HandleFunc("GET /settlement/next-window", s.handleSettlementNextWindow)
	mux.HandleFunc("GET /settlement/deferred", s.handleListSettlementDeferrals)
	mux.HandleFunc("POST /settlement/advance", s.handleAdvanceSettlementWindows)
	mux.HandleFunc("POST /settlement/deferred/{id}/settle", s.handleSettleDeferredTransfer)
	mux.HandleFunc("GET /aging", s.handleAging)
	mux.HandleFunc("GET /liquidity/intraday", s.handleIntradayLiquidity)
	mux.HandleFunc("POST /holds", s.handleCreateHold)
	mux.HandleFunc("POST /holds/expire", s.handleExpireHolds)
	// LG-129: transfer disputes — payer-initiated chargeback-style
	// claims that freeze the disputed amount on the payee via a hold.
	mux.HandleFunc("POST /disputes", s.handleRaiseDispute)
	mux.HandleFunc("POST /disputes/{id}/resolve", s.handleResolveDispute)
	mux.HandleFunc("GET /disputes/{id}", s.handleGetDispute)
	mux.HandleFunc("POST /term-deposits", s.handleOpenTermDeposit)
	mux.HandleFunc("POST /term-deposits/sweep", s.handleSweepTermDeposits)
	mux.HandleFunc("GET /term-deposits/{id}", s.handleGetTermDeposit)
	mux.HandleFunc("POST /term-deposits/{id}/accrue", s.handleAccrueTermInterest)
	mux.HandleFunc("POST /term-deposits/{id}/mature", s.handleMatureTermDeposit)
	mux.HandleFunc("POST /term-deposits/{id}/withdraw", s.handleWithdrawTermDeposit)
	mux.HandleFunc("POST /credit-lines", s.handleGrantCreditLine)
	mux.HandleFunc("POST /credit-lines/reduce", s.handleReduceCreditLine)
	mux.HandleFunc("GET /credit-lines", s.handleGetCreditLine)
	mux.HandleFunc("POST /credit-lines/accrue-interest", s.handleAccrueCreditInterest)
	mux.HandleFunc("POST /ecl/exposures", s.handleRegisterECLExposure)
	mux.HandleFunc("POST /ecl/exposures/dpd", s.handleSetECLDaysPastDue)
	mux.HandleFunc("GET /ecl/exposures", s.handleListECLExposures)
	mux.HandleFunc("POST /ecl/provisions", s.handleBookECLProvision)
	mux.HandleFunc("POST /ecl/config", s.handleSetECLStageConfig)
	mux.HandleFunc("POST /holds/{id}/capture", s.handleCaptureHold)
	mux.HandleFunc("POST /holds/{id}/capture/partial", s.handlePartialCaptureHold)
	mux.HandleFunc("POST /holds/{id}/release", s.handleReleaseHold)
	mux.HandleFunc("POST /accounts/{id}/freeze", s.handleFreezeAccount)
	mux.HandleFunc("POST /accounts/{id}/unfreeze", s.handleUnfreezeAccount)
	mux.HandleFunc("POST /accounts/{id}/suspend", s.handleSuspendAccount)
	mux.HandleFunc("POST /accounts/{id}/resume", s.handleResumeAccount)
	mux.HandleFunc("POST /virtual-accounts", s.handleRegisterVirtualAccount)
	mux.HandleFunc("GET /virtual-accounts", s.handleListVirtualAccounts)
	mux.HandleFunc("DELETE /virtual-accounts/{van}", s.handleUnregisterVirtualAccount)
	mux.HandleFunc("POST /accounts/{id}/close", s.handleCloseAccount)
	mux.HandleFunc("PUT /rules/{id}", s.handleSetJournalRule)
	mux.HandleFunc("DELETE /rules/{id}", s.handleDeleteJournalRule)
	mux.HandleFunc("GET /rules", s.handleListJournalRules)
	mux.HandleFunc("POST /periods/{id}/close", s.handleClosePeriod)
	mux.HandleFunc("POST /periods/{id}/preflight", s.handlePeriodPreflight)
	mux.HandleFunc("POST /periods/{id}/reopen", s.handleReopenPeriod)
	mux.HandleFunc("PUT /closing-accounts", s.handleSetClosingAccounts)
	mux.HandleFunc("GET /closing-accounts", s.handleGetClosingAccounts)
	mux.HandleFunc("POST /accounts/{id}/parent", s.handleSetParent)
	mux.HandleFunc("POST /denylist", s.handleAddDenylist)
	mux.HandleFunc("GET /denylist", s.handleListDenylist)
	mux.HandleFunc("DELETE /denylist/{value}", s.handleRemoveDenylist)
	mux.HandleFunc("POST /denylist/import", s.handleImportDenylist)
	mux.HandleFunc("GET /entries", s.handleListEntries)
	mux.HandleFunc("GET /entries/verify", s.handleVerifyEntries)
	mux.HandleFunc("POST /entries/anchor", s.handleAnchorCheckpoint)
	mux.HandleFunc("POST /entries/tsa-anchor", s.handleTSAAnchor)
	mux.HandleFunc("GET /audit/verify", s.handleAuditVerify)
	mux.HandleFunc("GET /audit/export", s.handleAuditExport)
	mux.HandleFunc("POST /api-keys", s.handleCreateAPIKey)
	mux.HandleFunc("GET /api-keys", s.handleListAPIKeys)
	mux.HandleFunc("GET /api-keys/{id}", s.handleGetAPIKey)
	mux.HandleFunc("POST /api-keys/{id}/rotate", s.handleRotateAPIKey)
	mux.HandleFunc("POST /api-keys/{id}/revoke", s.handleRevokeAPIKey)
	mux.HandleFunc("GET /events", s.handleOutboxEvents)
	mux.HandleFunc("GET /webhooks/deliveries", s.handleWebhookDeliveries)
	mux.HandleFunc("GET /accounts/{id}/balance", s.handleBalance)
	mux.HandleFunc("GET /accounts/{id}/entries", s.handleListAccountEntries)
	mux.HandleFunc("GET /accounts/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /accounts/{id}/merkle-proof", s.handleMerkleProof)
	mux.HandleFunc("GET /accounts/{id}/balance-at", s.handleBalanceAt)
	mux.HandleFunc("GET /accounts/{id}/ofx", s.handleOFXExport)
	mux.HandleFunc("GET /exports/pain001", s.handlePain001Export)
	mux.HandleFunc("GET /exports/pain001/{msgid}/status", s.handlePain001Status)
	mux.HandleFunc("POST /exports/pain001/{msgid}/receipts", s.handlePain001Receipts)
	mux.HandleFunc("POST /exports/pain001/{msgid}/settle", s.handlePain001Settle)
	mux.HandleFunc("GET /accounts/{id}/statement", s.handleStatement)
	mux.HandleFunc("POST /ofx/import", s.handleOFXImport)
	mux.HandleFunc("POST /camt053/import", s.handleCAMT053Import)
	mux.HandleFunc("GET /accounts/{id}/trial-balance", s.handleTrialBalance)
	mux.HandleFunc("GET /accounts/{id}/rollup", s.handleRollup)
	mux.HandleFunc("PUT /accounts/{id}/tags", s.handleSetAccountTags)
	mux.HandleFunc("PUT /accounts/{id}/alias", s.handleSetAlias)
	mux.HandleFunc("GET /accounts/{id}/alias", s.handleGetAlias)
	mux.HandleFunc("GET /accounts/{id}/external-mappings", s.handleListExternalMappings)
	mux.HandleFunc("POST /accounts/{id}/external-mappings", s.handleSetExternalMapping)
	mux.HandleFunc("DELETE /accounts/{id}/external-mappings", s.handleRemoveExternalMapping)
	mux.HandleFunc("GET /accounts/{id}/allowlist", s.handleGetTransferAllowlist)
	mux.HandleFunc("PUT /accounts/{id}/allowlist", s.handleSetTransferAllowlist)
	mux.HandleFunc("PUT /sweep-reserves", s.handleSetSweepReserve)
	mux.HandleFunc("GET /sweep-reserves", s.handleListSweepReserves)
	mux.HandleFunc("DELETE /sweep-reserves", s.handleClearSweepReserve)
	mux.HandleFunc("GET /rollup", s.handleTagRollup)
	mux.HandleFunc("GET /balance-history", s.handleBalanceHistory)
	mux.HandleFunc("GET /snapshots/diff", s.handleSnapshotDiff)
	// GET /tenants lists every provisioned tenant ID (the default tenant
	// is "" — see ledger.DefaultTenantID). It is registered on the inner
	// mux so the tenant middleware below never treats it as a tenant
	// path: "/tenants" carries no "/tenants/" prefix.
	mux.HandleFunc("GET /tenants", s.handleListTenants)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		// Ledger-sourced counters are aggregated across every
		// provisioned tenant: /metrics is a process-level scrape, and a
		// single-tenant deployment (whose only tenant is the default)
		// reports exactly what it always did.
		var auditWritten, auditDropped, outboxWritten, outboxDropped uint64
		var breaches, ruleLegs, freezeReleases, healthViolations uint64
		var deferredSettlements uint64
		var lowLiqAlerts uint64
		var paceAlerts uint64
		var vanAttributions uint64
		var fxStaleAlerts, fxExpiredAlerts, fxOverridden uint64
		var fxRefreshes, fxMoves, fxProviderFails uint64
		var fxInconsistent uint64
		var fxSpreadBookings uint64
		var fxSpreadCents int64
		var eclProvisions, eclMigrations uint64
		var dunningEscalations uint64
		var extMappingChanges uint64
		var entryLinkChanges uint64
		var mandateDebits uint64
		for _, l := range s.tenants.Ledgers() {
			if written, dropped, ok := l.AuditStats(); ok {
				auditWritten += written
				auditDropped += dropped
			}
			if written, dropped, ok := l.OutboxStats(); ok {
				outboxWritten += written
				outboxDropped += dropped
			}
			breaches += l.BalanceWatchBreachCount()
			paceAlerts += l.BudgetPaceAlertCount()
			lowLiqAlerts += l.LiquidityAlertCount()
			ruleLegs += l.RuleLegsCommitted()
			vanAttributions += l.VANAttributionCount()
			freezeReleases += l.FrozenAutoReleases()
		// The settlement-deferral counter likewise lives in the
		// ledger: deferrals are decided inside the PostTransfer
		// commit paths, not in a handler (see
		// ledger/settlement_cutoff.go, LG-101).
		deferredSettlements += l.SettlementDeferredCount()
			healthViolations += l.HealthViolationCount()
			fxStaleAlerts += l.FXRateStaleAlertCount()
			fxExpiredAlerts += l.FXRateExpiredAlertCount()
			fxOverridden += l.FXRateOverrideConversionCount()
			fxRefreshes += l.FXRateRefreshCount()
			fxMoves += l.FXRateMovedCount()
			fxProviderFails += l.FXRateProviderFailCount()
			fxInconsistent += l.FXRateInconsistencyAlertCount()
			fxSpreadBookings += l.FXSpreadBookingCount()
			fxSpreadCents += l.FXSpreadCentsTotal()
			eclProvisions += l.ECLProvisionCount()
			eclMigrations += l.ECLStageMigrationCount()
			dunningEscalations += l.DunningEscalationCount()
			extMappingChanges += l.ExternalMappingChangeCount()
			// The entry-link declaration counter likewise lives in the
			// ledger: links are declared inside DeclareEntryLink and
			// the reversal paths, not in a handler (see
			// ledger/links.go, LG-123).
			entryLinkChanges += l.EntryLinkChangeCount()
			// The mandate debit counter likewise lives in the ledger:
			// debits post inside DebitWithMandate, not in a handler
			// (see ledger/mandate.go, LG-125).
			mandateDebits += l.MandateDebitCount()
		}
		s.metrics.AuditEventsTotal.Store(auditWritten)
		s.metrics.AuditDroppedTotal.Store(auditDropped)
		s.metrics.OutboxEventsTotal.Store(outboxWritten)
		s.metrics.OutboxDroppedTotal.Store(outboxDropped)
		// The balance-watch breach counter lives in the ledger too (the
		// alert fires inside Post/Transfer/..., not in a handler).
		s.metrics.BalanceWatchBreaches.Store(breaches)
		// The budget pace alert counter likewise lives in the
		// ledger: alerts fire inside BudgetPace, not in a handler.
		s.metrics.BudgetPaceAlertsTotal.Store(paceAlerts)
		// The low-liquidity alert counter likewise lives in the ledger:
		// alerts fire inside EvaluateLiquidityAlerts, not in a handler.
		s.metrics.LowLiquidityAlerts.Store(lowLiqAlerts)
		// The rules-engine leg counter likewise lives in the ledger: the
		// legs commit inside Post, not in a handler.
		s.metrics.JournalRuleLegsTotal.Store(ruleLegs)
		// The VAN attribution counter likewise lives in the ledger:
		// attributions apply inside Post/Transfer/..., not in a
		// handler.
		s.metrics.VANAttributionsTotal.Store(vanAttributions)
		// The freeze auto-release counter lives in the ledger too:
		// releases happen inside the write path and the background
		// sweeper, not in a handler.
		s.metrics.FreezeAutoReleasesTotal.Store(freezeReleases)
		// The settlement-deferral counter is mirrored like the other
		// ledger-sourced counters above.
		s.metrics.SettlementDeferredTotal.Store(deferredSettlements)
		// The balance-health violation counter lives in the ledger too:
		// violations are detected by the background watchdog inside the
		// ledger package, not in a handler.
		s.metrics.BalanceHealthViolations.Store(healthViolations)
		// The FX-rate watch alert counters likewise live in the ledger:
		// alerts are edge-triggered by the background watcher inside the
		// ledger package, not in a handler.
		s.metrics.FXRateStaleTotal.Store(fxStaleAlerts)
		s.metrics.FXRateExpiredTotal.Store(fxExpiredAlerts)
		// The manual-override conversion counter likewise lives in the
		// ledger: conversions price inside the FX write path, not in a
		// handler.
		s.metrics.FXRateOverriddenTotal.Store(fxOverridden)
		// The market-provider refresh counters likewise live in the
		// ledger: refreshes install inside the watcher's tick, not in a
		// handler.
		s.metrics.FXRateRefreshesTotal.Store(fxRefreshes)
		s.metrics.FXRateMovedTotal.Store(fxMoves)
		s.metrics.FXRateProviderFailedTotal.Store(fxProviderFails)
		// The FX-consistency alert counter likewise lives in the
		// ledger: inconsistencies are edge-triggered by the background
		// consistency watcher inside the ledger package.
		s.metrics.FXRateInconsistencyTotal.Store(fxInconsistent)
		// The FX-spread booking counters likewise live in the ledger:
		// spread legs journal inside the FX write paths, not in a
		// handler (see ledger/fx_spread.go, LG-93).
		s.metrics.FXSpreadBookingsTotal.Store(fxSpreadBookings)
		s.metrics.FXSpreadCentsTotal.Store(fxSpreadCents)
		// The ECL provision and stage-migration counters likewise live
		// in the ledger: provisions commit inside BookECLProvision and
		// migrations record inside the staging paths, not in a handler.
		s.metrics.ECLProvisionsTotal.Store(eclProvisions)
		s.metrics.ECLStageMigrationsTotal.Store(eclMigrations)
		// The dunning escalation counter likewise lives in the ledger:
		// escalations fire inside EscalateDunning, not in a handler.
		s.metrics.DunningEscalationsTotal.Store(dunningEscalations)
		// The external-mapping change counter likewise lives in the
		// ledger: mappings are declared inside SetExternalMapping /
		// RemoveExternalMapping, not in a handler.
		s.metrics.ExternalMappingChangesTotal.Store(extMappingChanges)
		s.metrics.EntryLinkChangesTotal.Store(entryLinkChanges)
		s.metrics.MandateDebitsTotal.Store(mandateDebits)
		// The tenant count is process-level, like the counters above.
		s.metrics.TenantsTotal.Store(uint64(s.tenants.Count()))
		// The audit-sink counters live in the sink itself (it is the
		// only producer); /metrics mirrors them when the sink runs.
		if s.auditSink != nil {
			sent, failed, dead := s.auditSink.Stats()
			s.metrics.AuditSinkSentTotal.Store(sent)
			s.metrics.AuditSinkFailedAttemptsTotal.Store(failed)
			s.metrics.AuditSinkDeadLettersTotal.Store(dead)
		}
		// The webhook failure counter likewise lives in the
		// dispatcher; /metrics mirrors it when webhooks run.
		if s.webhooks != nil {
			_, _, failures, _ := s.webhooks.Stats()
			s.metrics.WebhookFailuresTotal.Store(failures)
		}
		s.metrics.handleMetrics(w, r)
	})
	return s.tenantMiddleware(s.apiKeyMiddleware(mux))
}

// apiKeyCtxKey carries the authenticated API key through the request
// context (see apiKeyMiddleware).
type apiKeyCtxKey struct{}

// apiKeyRequiredScope maps a request to the API key scope it needs
// (LG-130). Key management is always admin:keys; reads need
// read:ledger; journal-booking paths need their write scope; review
// decisions and every other mutating operator path need admin:audit.
func apiKeyRequiredScope(method, path string) string {
	if strings.HasPrefix(path, "/api-keys") {
		return ledger.ScopeAdminKeys
	}
	if path == "/metrics" {
		return ledger.ScopeReadLedger
	}
	if method == http.MethodGet || method == http.MethodHead {
		return ledger.ScopeReadLedger
	}
	if strings.HasPrefix(path, "/entries") {
		return ledger.ScopeWriteEntries
	}
	if strings.HasPrefix(path, "/transfers") {
		for _, op := range []string{"/approve", "/reject", "/expire"} {
			if strings.Contains(path, op) {
				return ledger.ScopeAdminAudit
			}
		}
		return ledger.ScopeWriteTransfer
	}
	return ledger.ScopeAdminAudit
}

// apiKeyMiddleware enforces API key authentication (LG-130). It is a
// pure pass-through until the ledger enables enforcement with
// ledger.WithAPIKeyAuth, so the anonymous-operator flow and every
// existing test keep working unchanged. Inside tenantMiddleware, so
// tenant resolution (and its path stripping) already happened and
// s.ledgerFor resolves the right per-tenant key registry.
func (s *server) apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := s.ledgerFor(r)
		if !l.APIKeyAuthEnabled() {
			next.ServeHTTP(w, r)
			return
		}
		secret := r.Header.Get("X-API-Key")
		if secret == "" {
			// Bootstrap: with zero keys issued, POST /api-keys is open
			// so the first operator key can be minted; every other
			// route is 401.
			if r.Method == http.MethodPost && r.URL.Path == "/api-keys" && l.APIKeyCount() == 0 {
				next.ServeHTTP(w, r)
				return
			}
			s.metrics.APIKeyAuthFailuresTotal.Add(1)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing X-API-Key header"})
			return
		}
		key, err := l.AuthenticateAPIKey(secret)
		if err != nil {
			s.metrics.APIKeyAuthFailuresTotal.Add(1)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key"})
			return
		}
		if need := apiKeyRequiredScope(r.Method, r.URL.Path); !key.HasScope(need) {
			s.metrics.APIKeyAuthFailuresTotal.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "API key lacks required scope: " + need})
			return
		}
		if !l.ConsumeAPIKeyQuota(key.ID) {
			s.metrics.APIKeyAuthFailuresTotal.Add(1)
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "API key rate quota exceeded"})
			return
		}
		// Attribute the audit trail to the key when the caller did not
		// name an explicit operator via x-ledger-actor.
		if r.Header.Get("x-ledger-actor") == "" {
			r = r.Clone(r.Context())
			r.Header.Set("X-Ledger-Actor", "apikey:"+key.Name)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey{}, key)))
	})
}

// writeAPIKeyError maps ledger API key errors to HTTP status codes.
func writeAPIKeyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrAPIKeyNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ledger.ErrAPIKeyInvalid), errors.Is(err, ledger.ErrAPIKeyExpired):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
	case errors.Is(err, ledger.ErrAPIKeyRevoked):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}

type createAPIKeyRequest struct {
	Name            string     `json:"name"`
	Scopes          []string   `json:"scopes"`
	ExpiresAt       *time.Time `json:"expires_at"`
	RateLimitPerMin int64      `json:"rate_limit_per_min"`
}

// handleCreateAPIKey implements POST /api-keys: mint a new API key.
// The secret is returned exactly once in this response — the ledger
// stores only its SHA-256 hash and can never reproduce it. Requires
// the admin:keys scope, except for the bootstrap call (zero keys
// issued yet), which the middleware lets through unauthenticated.
func (s *server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req createAPIKeyRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	var exp time.Time
	if req.ExpiresAt != nil {
		exp = *req.ExpiresAt
	}
	k, secret, err := s.ledgerFor(r).CreateAPIKeyCtx(actx, req.Name, req.Scopes, exp, req.RateLimitPerMin)
	if err != nil {
		writeAPIKeyError(w, err)
		return
	}
	s.metrics.APIKeyCreationsTotal.Add(1)
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": secret})
}

// handleListAPIKeys implements GET /api-keys: every key's public
// record (secret material never included). Requires admin:keys.
func (s *server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": s.ledgerFor(r).ListAPIKeys()})
}

// handleGetAPIKey implements GET /api-keys/{id}. Requires admin:keys.
func (s *server) handleGetAPIKey(w http.ResponseWriter, r *http.Request) {
	k, ok := s.ledgerFor(r).GetAPIKey(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": ledger.ErrAPIKeyNotFound.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": k})
}

type rotateAPIKeyRequest struct {
	// GraceSeconds is how long the old secret keeps working after the
	// rotation (dual-run for rolling deployments). Zero retires it at
	// once.
	GraceSeconds int64 `json:"grace_seconds"`
}

// handleRotateAPIKey implements POST /api-keys/{id}/rotate: issue a
// fresh secret for the same logical identity. The old secret stays
// valid for grace_seconds. Requires admin:keys.
func (s *server) handleRotateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req rotateAPIKeyRequest
	if r.ContentLength != 0 {
		if !s.decodeJSONBody(w, r, &req) {
			return
		}
	}
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	k, secret, err := s.ledgerFor(r).RotateAPIKeyCtx(actx, r.PathValue("id"), time.Duration(req.GraceSeconds)*time.Second)
	if err != nil {
		writeAPIKeyError(w, err)
		return
	}
	s.metrics.APIKeyCreationsTotal.Add(1)
	writeJSON(w, http.StatusOK, map[string]any{"key": k, "secret": secret})
}

// handleRevokeAPIKey implements POST /api-keys/{id}/revoke: retire a
// key immediately. Idempotent. Requires admin:keys.
func (s *server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	actx, ok := s.actorCtx(w, r)
	if !ok {
		return
	}
	k, err := s.ledgerFor(r).RevokeAPIKeyCtx(actx, r.PathValue("id"))
	if err != nil {
		writeAPIKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": k})
}

// tenantCtxKey carries the resolved tenant ID through the request
// context from the tenant middleware to ledgerFor.
type tenantCtxKey struct{}

// tenantFromContext returns the tenant ID the middleware resolved for
// the request, or false when the request named no tenant (default
// tenant).
func tenantFromContext(ctx context.Context) (ledger.TenantID, bool) {
	id, ok := ctx.Value(tenantCtxKey{}).(ledger.TenantID)
	return id, ok
}

// tenantMiddleware resolves multi-tenant routing before the inner mux
// sees the request. A path of the form /tenants/{id}/... selects the
// tenant and the remainder of the path is dispatched to the ordinary
// routes (so /tenants/acme/entries behaves exactly like /entries, but
// against acme's isolated ledger). Otherwise the x-tenant header selects
// the tenant. A request that names no tenant is dispatched unchanged to
// the default tenant. An invalid tenant ID is rejected with 400 before
// any ledger is touched; unknown (never-before-seen) IDs are provisioned
// lazily by the tenant store on first use.
func (s *server) tenantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var tenantID string
		if rest, ok := strings.CutPrefix(r.URL.Path, "/tenants/"); ok {
			seg, remainder, _ := strings.Cut(rest, "/")
			if err := ledger.ValidateTenantID(seg); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			tenantID = seg
			r = r.Clone(r.Context())
			r.URL.Path = "/" + remainder
		} else if h := r.Header.Get("X-Tenant"); h != "" {
			if err := ledger.ValidateTenantID(h); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			tenantID = h
		}
		if tenantID != "" {
			ctx := context.WithValue(r.Context(), tenantCtxKey{}, ledger.TenantID(tenantID))
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// handleListTenants answers GET /tenants with every provisioned tenant
// ID, sorted. The default tenant reports as "".
func (s *server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	ids := s.tenants.IDs()
	strs := make([]string, 0, len(ids))
	for _, id := range ids {
		strs = append(strs, string(id))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": strs})
}

// defaultMaxBodyBytes caps a single POST /entries body. Entry payloads are
// tiny (a few hundred bytes), so 1 MiB is generous while still bounding the
// memory one request can force the server to buffer.
const defaultMaxBodyBytes = 1 << 20

// maxRequestBodyBytes reads LEDGER_MAX_BODY_BYTES (a byte count, e.g.
// "1048576"). Unset or invalid values fall back to the default with a log
// line, mirroring shutdownTimeout.
func maxRequestBodyBytes() int64 {
	raw := os.Getenv("LEDGER_MAX_BODY_BYTES")
	if raw == "" {
		return defaultMaxBodyBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_MAX_BODY_BYTES %q, using %d", raw, defaultMaxBodyBytes)
		return defaultMaxBodyBytes
	}
	return n
}

// parseEntryListQuery parses the ?since=&until=&limit= query shared by the
// journal-export endpoints. since/until are RFC3339 timestamps filtering
// CreatedAt in [since, until): omitted since means the beginning of time,
// omitted until means no upper bound. limit defaults to 100. Malformed
// values return an error the handlers render as 400.
func parseEntryListQuery(q url.Values) (since, until time.Time, limit int, err error) {
	since = time.Time{}
	if v := firstQuery(q, "since"); v != "" {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			return time.Time{}, time.Time{}, 0, errors.New("invalid since timestamp (want RFC3339)")
		}
		since = t
	}
	until = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if v := firstQuery(q, "until"); v != "" {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			return time.Time{}, time.Time{}, 0, errors.New("invalid until timestamp (want RFC3339)")
		}
		until = t
	}
	limit = 100
	if v := firstQuery(q, "limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil {
			return time.Time{}, time.Time{}, 0, errors.New("invalid limit (want integer)")
		}
		limit = n
	}
	return since, until, limit, nil
}

func firstQuery(q url.Values, key string) string {
	if vs, ok := q[key]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// handleListAccountEntries implements GET /accounts/{id}/entries:
// per-account, time-windowed, cursor-paginated journal export.
//
//	GET /accounts/{id}/entries?since=<rfc3339>&until=<rfc3339>&limit=100&cursor=<opaque>
//
// The pagination contract mirrors GET /entries (same sorting, same cursor
// semantics, same 400s). Unknown accounts return an empty page, not 404 —
// the journal is append-only, so absence means "nothing yet".
func (s *server) handleListAccountEntries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	account, _ := s.resolveAccountRef(r, id)
	since, until, limit, err := parseEntryListQuery(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	page, next, err := s.ledgerFor(r).ListAccountEntries(account, since, until, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":     page,
		"next_cursor": next,
	})
}

// handleListEntries implements GET /entries: time-windowed, cursor-paginated
// export of the journal.
//
//	GET /entries?since=<rfc3339>&until=<rfc3339>&limit=100&cursor=<opaque>
//
// since/until are RFC3339 timestamps filtering CreatedAt in [since, until).
// Omitted since means the beginning of time; omitted until means no upper
// bound. limit defaults to 100 and is capped at 1000. The response is
// {"entries":[...], "next_cursor":"..."}; an empty next_cursor marks the last
// page. Malformed timestamps, cursors, or limits return 400. An optional
// ?memo= keyword filters to entries whose memo note contains it as a
// case-sensitive substring; an optional ?ref= filters to the single entry
// whose external_ref equals it exactly; optional ?meta.<key>=<value>
// parameters filter to entries carrying every given metadata pair (see
// JournalEntry.Metadata). All filters compose with the time window and
// cursor pagination (see ListEntriesMetadataFiltered).
func (s *server) handleListEntries(w http.ResponseWriter, r *http.Request) {
	since, until, limit, err := parseEntryListQuery(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	q := r.URL.Query()
	var meta map[string]string
	for name, values := range q {
		key, ok := strings.CutPrefix(name, "meta.")
		if !ok || key == "" || len(values) == 0 {
			continue
		}
		if meta == nil {
			meta = make(map[string]string)
		}
		meta[key] = values[0]
	}

	page, next, err := s.ledgerFor(r).ListEntriesMetadataFiltered(since, until, q.Get("cursor"), limit, q.Get("memo"), q.Get("ref"), meta)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":     page,
		"next_cursor": next,
	})
}

func main() {
	addr := os.Getenv("PORT")
	if addr == "" {
		addr = "8080"
	}
	if !strings.HasPrefix(addr, ":") {
		addr = ":" + addr
	}

	// LEDGER_IDEMPOTENCY_TTL bounds the idempotency-key index in memory
	// (e.g. "72h"). Unset or unparsable means keys never expire.
	var opts []ledger.Option
	if raw := os.Getenv("LEDGER_IDEMPOTENCY_TTL"); raw != "" {
		if ttl, err := time.ParseDuration(raw); err != nil {
			log.Printf("ledger-api-go: ignoring invalid LEDGER_IDEMPOTENCY_TTL %q: %v", raw, err)
		} else if ttl > 0 {
			opts = append(opts, ledger.WithIdempotencyTTL(ttl))
			log.Printf("ledger-api-go: idempotency key TTL = %v", ttl)
		}
	}

	// LEDGER_NO_OVERDRAFT_ACCOUNTS is a comma-separated list of account IDs
	// guarded against overdrafts from the start (e.g. "cust-123,cust-456").
	// Postings that would take one of these accounts below zero are
	// rejected with 422. Whitespace around IDs is trimmed; empty entries
	// are ignored.
	if raw := os.Getenv("LEDGER_NO_OVERDRAFT_ACCOUNTS"); raw != "" {
		var protected []ledger.AccountID
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				protected = append(protected, ledger.AccountID(id))
			}
		}
		if len(protected) > 0 {
			opts = append(opts, ledger.WithOverdraftProtection(protected...))
			log.Printf("ledger-api-go: overdraft protection enabled for %d account(s)", len(protected))
		}
	}

	// LEDGER_TRANSFER_FEE configures the default transfer fee policy.
	// Flat form (legacy): "<rateBps>:<revenueAccount>" (e.g.
	// "250:fee-revenue" for a flat 2.5%). Tiered form:
	// "<min>:<bps>,<min>:<bps>,...@<revenueAccount>" (e.g.
	// "0:0,10000:250,1000000:100@fee-revenue" for fee-free dust, 2.5%
	// from $100, 1% from $10k). Unless a transfer carries an explicit
	// fee or sets skip_fee, POST /transfers books floor(amount *
	// tierRateBps / 10000) cents to the revenue account on top of the
	// transfer amount; the receipt discloses the applied tier
	// (fee_tier_index) and rate (fee_rate_bps). Unset means no default
	// fee. An invalid value fails the startup fast (log.Fatal): a
	// misconfigured fee schedule must never silently misprice transfers.
	if raw := os.Getenv("LEDGER_TRANSFER_FEE"); raw != "" {
		tiers, account, err := ledger.ParseFeeSchedule(raw)
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		opts = append(opts, ledger.WithTransferFeeSchedule(tiers, account))
		log.Printf("ledger-api-go: transfer fee schedule = %v to %q", tiers, account)
	}

	// LEDGER_MAX_TRANSFER_CHUNK sets the opt-in per-transfer chunk cap in
	// cents for split transfers (see ledger.WithMaxTransferChunkCents):
	// a transfer above the cap is split into atomic chunks of at most
	// this size (PostTransferSplit). Unset means no chunking. An invalid
	// value fails the startup fast (log.Fatal): a misconfigured cap must
	// never silently disable splitting.
	if raw := os.Getenv("LEDGER_MAX_TRANSFER_CHUNK"); raw != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || n < 0 {
			log.Fatalf("ledger-api-go: invalid LEDGER_MAX_TRANSFER_CHUNK %q: want a non-negative integer (cents)", raw)
		}
		opts = append(opts, ledger.WithMaxTransferChunkCents(n))
		log.Printf("ledger-api-go: max transfer chunk = %d cents", n)
	}

	// LEDGER_DAILY_OUTFLOW_LIMITS configures per-account per-currency daily
	// outflow limits (see ledger.ParseDailyLimits for the syntax, e.g.
	// "cust-123:USD:100000,cust-456:EUR:50000"). Postings that would take
	// an account's cumulative outflow for the UTC day above its limit are
	// rejected with 422. Unset means no limits. An invalid value fails the
	// startup fast (log.Fatal): a misconfigured risk control must never
	// silently run unenforced.
	if raw := os.Getenv("LEDGER_DAILY_OUTFLOW_LIMITS"); raw != "" {
		limits, err := ledger.ParseDailyLimits(raw)
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		for _, dl := range limits {
			opts = append(opts, ledger.WithDailyLimit(dl.Account, dl.Currency, dl.LimitCents))
		}
		log.Printf("ledger-api-go: daily outflow limits = %d configured", len(limits))
	}

	// LEDGER_MONTHLY_SPEND_BUDGETS configures per-account per-currency
	// monthly spend budgets (see ledger.ParseAccountBudgets for the
	// syntax, e.g. "cust-123:USD:500000,cust-456:EUR:200000"). Postings
	// that would take an account's cumulative spend for the UTC calendar
	// month above its budget are rejected with 422. Unset means no
	// budgets. An invalid value fails the startup fast (log.Fatal): a
	// misconfigured risk control must never silently run unenforced.
	if raw := os.Getenv("LEDGER_MONTHLY_SPEND_BUDGETS"); raw != "" {
		budgets, err := ledger.ParseAccountBudgets(raw)
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		for _, b := range budgets {
			opts = append(opts, ledger.WithAccountBudget(b.Account, b.Currency, b.BudgetCents))
		}
		log.Printf("ledger-api-go: monthly spend budgets = %d configured", len(budgets))
	}

	// LEDGER_LOW_BALANCE_THRESHOLDS configures per-account per-currency
	// low-balance alert levels (see ledger.ParseLowBalanceThresholds for
	// the syntax, e.g. "cust-123:USD:50000,cust-456:EUR:-10000"). The
	// first posting that takes an account's balance below its level emits
	// one balance_threshold_breached audit event (direction "low"); the
	// alert is advisory and never rejects a posting. Unset means no alert
	// levels. An invalid value fails the startup fast (log.Fatal): a
	// misconfigured alert level must never silently run unenforced.
	// (Legacy form of LEDGER_BALANCE_WATCHES below: low side only.)
	if raw := os.Getenv("LEDGER_LOW_BALANCE_THRESHOLDS"); raw != "" {
		thresholds, err := ledger.ParseLowBalanceThresholds(raw)
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		for _, lt := range thresholds {
			opts = append(opts, ledger.WithLowBalanceThreshold(lt.Account, lt.Currency, lt.ThresholdCents))
		}
		log.Printf("ledger-api-go: low-balance thresholds = %d configured", len(thresholds))
	}

	// LEDGER_BALANCE_WATCHES configures per-account per-currency balance
	// threshold watches with a low and/or a high level (see
	// ledger.ParseBalanceWatches for the syntax, e.g.
	// "cust-123:USD:50000:,ops:EUR::-10000,treasury:USD:100000:5000000":
	// an empty side leaves it unwatched). The first committed posting
	// that takes a balance outside its band emits one
	// balance_threshold_breached audit event with the direction; the
	// alert is advisory and never rejects a posting. Unset means no
	// watches. An invalid value fails the startup fast (log.Fatal), like
	// every other risk control. When both variables configure the same
	// (account, currency), this one wins (it is applied last).
	if raw := os.Getenv("LEDGER_BALANCE_WATCHES"); raw != "" {
		watches, err := ledger.ParseBalanceWatches(raw)
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		for _, w := range watches {
			opts = append(opts, ledger.WithBalanceWatch(w.Account, w.Currency, ledger.BalanceWatchConfig{
				LowCents:  w.LowCents,
				HighCents: w.HighCents,
			}))
		}
		log.Printf("ledger-api-go: balance watches = %d configured", len(watches))
	}

	// LEDGER_REVIEW_THRESHOLD configures the ledger-wide large-transfer
	// dual-control threshold in cents (e.g. "100000" for $1,000.00): a
	// transfer whose principal amount is at or above the threshold does
	// not settle — it enters pending_review, freezing the payer's funds
	// until an operator approves (POST /transfers/{id}/approve) or
	// rejects (POST /transfers/{id}/reject) it (see ledger.WithReviewThreshold).
	// Unset or "0" means disabled: no transfer is ever reviewed. An
	// invalid value fails the startup fast (log.Fatal): a misconfigured
	// risk control must never silently run unenforced.
	if raw := os.Getenv("LEDGER_REVIEW_THRESHOLD"); raw != "" {
		threshold, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || threshold < 0 {
			log.Fatalf("ledger-api-go: invalid LEDGER_REVIEW_THRESHOLD %q: want a non-negative integer (cents)", raw)
		}
		if threshold > 0 {
			opts = append(opts, ledger.WithReviewThreshold(threshold))
			log.Printf("ledger-api-go: transfer review threshold = %d cents", threshold)
		}
	}

	// LEDGER_REVIEW_THRESHOLDS configures per-account review thresholds
	// (see ledger.ParseReviewThresholds for the syntax, e.g.
	// "treasury:1000000,ops:500000"): a per-account threshold wins over
	// LEDGER_REVIEW_THRESHOLD for that account's outbound transfers.
	// Unset means no per-account thresholds. An invalid value fails the
	// startup fast (log.Fatal), like every other risk control.
	if raw := os.Getenv("LEDGER_REVIEW_THRESHOLDS"); raw != "" {
		thresholds, err := ledger.ParseReviewThresholds(raw)
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		opts = append(opts, ledger.WithReviewThresholds(thresholds))
		log.Printf("ledger-api-go: per-account review thresholds = %d configured", len(thresholds))
	}

	// LEDGER_LARGE_TX_THRESHOLDS configures per-currency large-transaction
	// (CTR) reporting thresholds (see ledger.ParseLargeTxThresholds for the
	// syntax, e.g. "USD:1000000,EUR:900000"): an account's UTC-day inflow
	// or outflow in that currency at/above the threshold generates a
	// report into the structured audit chain on POST
	// /compliance/large-tx-reports. Unset means no reporting. An invalid
	// value fails the startup fast (log.Fatal), like every other risk
	// control.
	if raw := os.Getenv("LEDGER_LARGE_TX_THRESHOLDS"); raw != "" {
		thresholds, err := ledger.ParseLargeTxThresholds(raw)
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		m := make(map[string]int64, len(thresholds))
		for _, t := range thresholds {
			m[t.Currency] = t.ThresholdCents
		}
		opts = append(opts, ledger.WithLargeTxThresholds(m))
		log.Printf("ledger-api-go: large-transaction thresholds = %d configured", len(thresholds))
	}

	// LEDGER_LARGE_TX_REPORT_CURRENCY sets the currency converted
	// large-transaction report amounts are disclosed in (default USD).
	// An invalid value fails the startup fast (log.Fatal): a
	// misconfigured compliance report must never silently run in the
	// wrong currency.
	if raw := os.Getenv("LEDGER_LARGE_TX_REPORT_CURRENCY"); raw != "" {
		opts = append(opts, ledger.WithLargeTxReportCurrency(raw))
		log.Printf("ledger-api-go: large-transaction report currency = %s", raw)
	}

	// LEDGER_BUSINESS_DAY_WEEKEND and LEDGER_BUSINESS_DAY_HOLIDAYS
	// configure the business-day calendar (see
	// ledger.ParseBusinessDayWeekend / ParseBusinessDayHolidays, e.g.
	// WEEKEND="Fri,Sat" HOLIDAYS="2026-01-01,2026-10-01"): the weekly
	// rest days plus extra non-business dates. Unset means the default
	// Saturday/Sunday calendar with no custom holidays. Invalid values
	// fail the startup fast (log.Fatal), like every other calendar
	// input — interest accrual and schedule settlement must never run
	// on a silently wrong calendar.
	if rawW := os.Getenv("LEDGER_BUSINESS_DAY_WEEKEND"); rawW != "" || os.Getenv("LEDGER_BUSINESS_DAY_HOLIDAYS") != "" {
		weekend, err := ledger.ParseBusinessDayWeekend(rawW)
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		holidays, err := ledger.ParseBusinessDayHolidays(os.Getenv("LEDGER_BUSINESS_DAY_HOLIDAYS"))
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		opts = append(opts, ledger.WithBusinessDayCalendar(weekend, holidays))
		log.Printf("ledger-api-go: business-day calendar configured (weekend=%q holidays=%d)", rawW, len(holidays))
	}

	// LEDGER_ACCRUAL_BUSINESS_DAYS_ONLY makes interest and fee accruals
	// business-day-only ("1"/"true"): a non-business-day asof is
	// rejected instead of booked. Unset (or anything else) means
	// calendar-day accruals, the historical behavior.
	if raw := strings.ToLower(strings.TrimSpace(os.Getenv("LEDGER_ACCRUAL_BUSINESS_DAYS_ONLY"))); raw == "1" || raw == "true" {
		opts = append(opts, ledger.WithAccrualBusinessDaysOnly(true))
		log.Printf("ledger-api-go: accruals restricted to business days")
	}

	// LEDGER_REVIEW_EXPIRY bounds how long a pending review may wait for
	// an operator decision (a Go duration string, e.g. "24h"): lapsed
	// reviews are auto-rejected by ExpireReviews (see
	// ledger.WithReviewExpiry). Unset means reviews never lapse on their
	// own; invalid values are ignored with a log line (the sweeper stays
	// disabled), like the sweep intervals below.
	if raw := os.Getenv("LEDGER_REVIEW_EXPIRY"); raw != "" {
		if d, err := time.ParseDuration(raw); err != nil {
			log.Printf("ledger-api-go: ignoring invalid LEDGER_REVIEW_EXPIRY %q: %v", raw, err)
		} else if d > 0 {
			opts = append(opts, ledger.WithReviewExpiry(d))
			log.Printf("ledger-api-go: transfer review expiry = %v", d)
		}
	}

	// LEDGER_FX_ACCOUNT configures the ledger-wide FX clearing account,
	// the counterparty of cross-currency transfer legs (see
	// ledger.WithFXAccount). Unset means cross-currency transfers must
	// carry fx_account on each request.
	if raw := os.Getenv("LEDGER_FX_ACCOUNT"); raw != "" {
		opts = append(opts, ledger.WithFXAccount(ledger.AccountID(raw)))
		log.Printf("ledger-api-go: FX clearing account = %q", raw)
	}

	// LEDGER_FX_INCOME_ACCOUNT configures the ledger-wide FX
	// spread-income account, the credit counterparty of cross-currency
	// spread legs (see ledger.WithFXIncomeAccount, LG-93). Unset means
	// spread-earning transfers must carry fx_income_account on each
	// request (a positive spread with no usable income account is
	// rejected fail-closed).
	if raw := os.Getenv("LEDGER_FX_INCOME_ACCOUNT"); raw != "" {
		opts = append(opts, ledger.WithFXIncomeAccount(ledger.AccountID(raw)))
		log.Printf("ledger-api-go: FX spread income account = %q", raw)
	}

	// LEDGER_FX_RATES configures the FX rate table (see ledger.ParseFXRates
	// for the syntax, e.g. "USD:EUR=108/100,USD:CNY=720/100"). Rates take
	// effect at ledger version 0 (startup). Unset means no rates: any
	// cross-currency transfer is rejected with 422. An invalid value fails
	// the startup fast (log.Fatal): a misconfigured rate table must never
	// silently convert at a wrong rate.
	if raw := os.Getenv("LEDGER_FX_RATES"); raw != "" {
		rates, err := ledger.ParseFXRates(raw)
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		for _, r := range rates {
			opts = append(opts, ledger.WithFXRateOption(r.FromCurrency, r.ToCurrency, r.Num, r.Den))
		}
		log.Printf("ledger-api-go: FX rates = %d configured", len(rates))
	}

	// LEDGER_REVAL_ACCOUNT configures the P&L account that receives the
	// gain/loss legs of FX revaluations (see ledger.WithRevaluationAccount
	// and POST /revaluations). Unset means revaluation is disabled:
	// POST /revaluations is rejected with 400 until the operator
	// configures the account.
	if raw := os.Getenv("LEDGER_REVAL_ACCOUNT"); raw != "" {
		opts = append(opts, ledger.WithRevaluationAccount(ledger.AccountID(raw)))
		log.Printf("ledger-api-go: revaluation P&L account = %q", raw)
	}

	// LEDGER_TD_INTEREST_EXPENSE configures the ledger-wide
	// interest-expense account that term-deposit interest accruals
	// credit (see ledger.SetTermDepositInterestExpense and POST
	// /term-deposits/{id}/accrue). Unset means unconfigured: accruals
	// are fail-closed until the operator configures it.
	if raw := os.Getenv("LEDGER_TD_INTEREST_EXPENSE"); raw != "" {
		opts = append(opts, ledger.WithTermDepositInterestExpense(ledger.AccountID(raw)))
		log.Printf("ledger-api-go: term-deposit interest expense account = %q", raw)
	}

	// LEDGER_CREDIT_INTEREST_INCOME configures the ledger-wide
	// interest-income account that credit-line interest accruals debit
	// (see ledger.SetCreditInterestIncome and POST
	// /credit-lines/accrue-interest). Unset means unconfigured:
	// accruals are fail-closed until the operator configures it.
	if raw := os.Getenv("LEDGER_CREDIT_INTEREST_INCOME"); raw != "" {
		opts = append(opts, ledger.WithCreditInterestIncome(ledger.AccountID(raw)))
		log.Printf("ledger-api-go: credit-line interest income account = %q", raw)
	}

	// LEDGER_ECL_EXPENSE configures the ledger-wide credit-loss expense
	// account that ECL provisions debit (see ledger.SetECLLossExpense
	// and POST /ecl/provisions). Unset means unconfigured: provisions
	// are fail-closed until the operator configures it.
	if raw := os.Getenv("LEDGER_ECL_EXPENSE"); raw != "" {
		opts = append(opts, ledger.WithECLLossExpense(ledger.AccountID(raw)))
		log.Printf("ledger-api-go: ECL loss expense account = %q", raw)
	}

	// LEDGER_ECL_ALLOWANCE configures the ledger-wide loss-allowance
	// account that ECL provisions credit (see ledger.SetECLAllowance
	// and POST /ecl/provisions). Unset means unconfigured: provisions
	// are fail-closed until the operator configures it.
	if raw := os.Getenv("LEDGER_ECL_ALLOWANCE"); raw != "" {
		opts = append(opts, ledger.WithECLAllowance(ledger.AccountID(raw)))
		log.Printf("ledger-api-go: ECL allowance account = %q", raw)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("ledger-api-go: listen %s: %v", addr, err)
	}

	// LEDGER_AUDIT_DIR enables the structured compliance audit log (see
	// ledger/audit.go): every mutating operation and every reconcile run
	// is appended as JSONL to the directory, with daily rotation and a
	// per-file size cap. Unset means disabled. LEDGER_AUDIT_MAX_BYTES
	// caps a single audit file (default 100 MiB). The log is closed
	// (flushed) on shutdown after the background workers stop.
	//
	// LEDGER_AUDIT_SIGNING_KEYS enables per-line HMAC-SHA256 signatures
	// (see ledger/audit_sign.go): "id1:hexkey,id2:hexkey@<retiredAt
	// RFC3339>". The first non-retired key signs new lines; retired keys
	// still verify inside LEDGER_AUDIT_SIGNING_GRACE (a Go duration
	// string, default 24h). Unset means the log runs unsigned and records
	// an audit_signing_disabled alert in the trail itself. An invalid
	// value fails startup fast: a misconfigured signing key must never
	// silently run.
	if dir := os.Getenv("LEDGER_AUDIT_DIR"); dir != "" {
		auditOpts := []ledger.AuditLogOption{ledger.WithAuditLogMaxBytes(auditMaxBytes())}
		if raw := os.Getenv("LEDGER_AUDIT_SIGNING_KEYS"); raw != "" {
			keys, err := ledger.ParseAuditSigningKeys(raw)
			if err != nil {
				log.Fatalf("ledger-api-go: %v", err)
			}
			auditOpts = append(auditOpts, ledger.WithAuditLogSigning(keys, auditSigningGrace()))
			log.Printf("ledger-api-go: audit log HMAC signing enabled (%d key(s))", len(keys))
		} else {
			log.Printf("ledger-api-go: audit log HMAC signing disabled: lines are unsigned; an audit_signing_disabled alert is recorded in the trail")
		}
		al, err := ledger.NewAuditLog(dir, auditOpts...)
		if err != nil {
			log.Fatalf("ledger-api-go: audit log: %v", err)
		}
		defer al.Close()
		opts = append(opts, ledger.WithAuditLog(al))
		log.Printf("ledger-api-go: audit log enabled (dir %s)", dir)
	}

	// LEDGER_OUTBOX_DIR enables the domain-event outbox (see
	// ledger/outbox.go): every committed state change is appended as
	// JSONL to the directory, one file per UTC day, fsync'd per event.
	// Unset means disabled. The outbox is closed (file handle released)
	// on shutdown after the background workers stop.
	if dir := os.Getenv("LEDGER_OUTBOX_DIR"); dir != "" {
		ob, err := ledger.NewOutbox(dir)
		if err != nil {
			log.Fatalf("ledger-api-go: outbox: %v", err)
		}
		defer ob.Close()
		opts = append(opts, ledger.WithOutbox(ob))
		log.Printf("ledger-api-go: domain event outbox enabled (dir %s)", dir)
	}

	// LEDGER_WEBHOOKS enables the outbound signed-webhook dispatcher
	// (LG-102, see ledger.ParseWebhookEndpoints for the JSON syntax: a
	// JSON array of endpoint objects with name, url, events, and
	// signing key material). Each registered endpoint receives the
	// outbox domain events its filter selects as HMAC-SHA256-signed
	// JSON POSTs. Unset means disabled. An invalid value fails the
	// startup fast (log.Fatal): a misconfigured webhook must never
	// silently run unsigned or unfiltered.
	//
	// Webhooks subscribe to the outbox stream, so LEDGER_OUTBOX_DIR is
	// required alongside: without an outbox there are no events to
	// forward, and starting eventless would silently drop the
	// operator's intent. Like WithOutbox, the dispatcher attaches to
	// every tenant ledger through the tenant store's shared options.
	var webhooks *ledger.WebhookDispatcher
	if raw := os.Getenv("LEDGER_WEBHOOKS"); raw != "" {
		if os.Getenv("LEDGER_OUTBOX_DIR") == "" {
			log.Fatalf("ledger-api-go: LEDGER_WEBHOOKS is set but LEDGER_OUTBOX_DIR is not: webhooks subscribe to outbox domain events, nothing to forward without an outbox")
		}
		cfgs, err := ledger.ParseWebhookEndpoints(raw)
		if err != nil {
			log.Fatalf("ledger-api-go: LEDGER_WEBHOOKS: %v", err)
		}
		webhooks, err = ledger.NewWebhookDispatcher(cfgs...)
		if err != nil {
			log.Fatalf("ledger-api-go: webhooks: %v", err)
		}
		opts = append(opts, ledger.WithWebhooks(webhooks))
		log.Printf("ledger-api-go: outbound webhooks enabled (%d endpoints)", len(cfgs))
	}

	// LEDGER_ANCHOR_KEY configures the Ed25519 key used to sign audit-chain
	// head checkpoints (see ledger.ParseAnchorKey: hex of a 32-byte seed
	// or a 64-byte private key). Anchoring itself is operator-driven via
	// POST /entries/anchor; checkpoints land in LEDGER_AUDIT_DIR as
	// checkpoints.jsonl. Unset means unconfigured. An invalid value fails
	// the startup fast (log.Fatal): a misconfigured signing key must never
	// silently run unanchored.
	if raw := os.Getenv("LEDGER_ANCHOR_KEY"); raw != "" {
		key, err := ledger.ParseAnchorKey(raw)
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		opts = append(opts, ledger.WithAnchorKey(key))
		log.Printf("ledger-api-go: anchor key configured")
	}

	// LEDGER_TSA_URL configures the RFC 3161 timestamp authority
	// endpoint for external audit-chain anchoring (LG-84): the
	// background worker countersigns the chain head on
	// LEDGER_TSA_INTERVAL. Unset means TSA anchoring is disabled.
	// LEDGER_TSA_CERT_FILE optionally pins the TSA trust anchor (PEM
	// certificate): verification then reports trust-pinned results;
	// without it, token signatures verify against the token's own
	// embedded certificate and are honestly reported untrusted. An
	// unreadable/invalid cert file fails the startup fast.
	if raw := os.Getenv("LEDGER_TSA_URL"); raw != "" {
		opts = append(opts, ledger.WithTSAURL(raw))
		log.Printf("ledger-api-go: TSA anchoring enabled (url %s)", raw)
	}
	if path := os.Getenv("LEDGER_TSA_CERT_FILE"); path != "" {
		pemBytes, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("ledger-api-go: read LEDGER_TSA_CERT_FILE: %v", err)
		}
		cert, err := ledger.ParseTSATrustCert(pemBytes)
		if err != nil {
			log.Fatalf("ledger-api-go: %v", err)
		}
		opts = append(opts, ledger.WithTSATrustCert(cert))
		log.Printf("ledger-api-go: TSA trust anchor configured")
	}

	// SIGINT/SIGTERM cancel the context; runServer then drains in-flight
	// requests instead of dropping them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := newServer(ledger.New(opts...), opts...)
	if webhooks != nil {
		srv.webhooks = webhooks
		defer webhooks.Stop()
	}

	// Optional background hold-expiry sweeper: periodically marks lapsed
	// holds expired so operators don't need to poll POST /holds/expire.
	// It shares the server context, so SIGINT/SIGTERM stops it, and every
	// tick is counted by the same ledger_hold_sweeps_total counter as the
	// operator endpoint. Unset or invalid means disabled.
	if interval, ok := holdSweepInterval(); ok {
		sweeper := ledger.StartHoldSweeper(ctx, srv.ledger, interval, func(int) {
			srv.metrics.HoldSweeps.Add(1)
		})
		log.Printf("ledger-api-go: hold sweep worker started (interval %v)", interval)
		defer sweeper.Stop()
	}

	// Optional background TSA anchoring worker (LG-84): periodically
	// countersigns the audit-chain head with the external RFC 3161
	// timestamp authority. It shares the server context, so
	// SIGINT/SIGTERM stops it. TSA outages never block posting — each
	// failure is recorded as a `tsa_anchor_failed` audit event (the
	// degraded alert) and counted in ledger_tsa_anchor_failures_total.
	// Unset URL or invalid interval means disabled.
	if tsaURL := os.Getenv("LEDGER_TSA_URL"); tsaURL != "" {
		if interval, ok := tsaAnchorInterval(); ok {
			authority := &tsaMetricsAuthority{
				inner:   &ledger.RFC3161Client{URL: tsaURL},
				metrics: srv.metrics,
			}
			anchorer := ledger.StartTimestampAnchoring(ctx, srv.ledger, interval, authority, func(anchor ledger.TimestampAnchor) {
				srv.metrics.TSAAnchorsTotal.Add(1)
			})
			log.Printf("ledger-api-go: TSA anchoring worker started (interval %v, url %s)", interval, tsaURL)
			defer anchorer.Stop()
		}
	}

	// Optional background review-expiry sweeper: auto-rejects pending
	// transfer reviews whose expiry has passed so frozen funds can never
	// strand in dual-control limbo. It shares the server context, so
	// SIGINT/SIGTERM stops it, and every tick's expired count feeds
	// ledger_review_rejected_total, the same counter the operator
	// endpoint increments. Unset or invalid means disabled.
	if interval, ok := reviewSweepInterval(); ok {
		sweeper := ledger.StartReviewSweeper(ctx, srv.ledger, interval, func(expired int) {
			srv.metrics.ReviewRejectedTotal.Add(uint64(expired))
		})
		log.Printf("ledger-api-go: review sweep worker started (interval %v)", interval)
		defer sweeper.Stop()
	}

	// Optional background schedule sweeper: fires due transfer schedules
	// (see ledger.TransferSchedule) so operators don't need to poll a
	// sweep endpoint. It shares the server context, so SIGINT/SIGTERM
	// stops it, and every fired run is counted by
	// ledger_scheduled_transfers_total. Unset or invalid means disabled.
	if interval, ok := scheduleSweepInterval(); ok {
		sweeper := ledger.StartScheduleSweeper(ctx, srv.ledger, interval, func(fired, _ int) {
			srv.metrics.ScheduledTransfersTotal.Add(uint64(fired))
		})
		log.Printf("ledger-api-go: schedule sweep worker started (interval %v)", interval)
		defer sweeper.Stop()
	}

	// Optional background report sweeper: generates due report
	// subscriptions (see ledger.ReportSchedule) so operators don't need
	// to poll a sweep endpoint. It shares the server context, so
	// SIGINT/SIGTERM stops it. Delivery failures are recorded on the
	// run and retried on the next sweep; the callback exists for
	// observability symmetry with the other sweepers. Unset or invalid
	// means disabled.
	if interval, ok := reportSweepInterval(); ok {
		sweeper := ledger.StartReportSweeper(ctx, srv.ledger, interval, func(generated, failed int) {
			// Log only productive ticks: a report sweeper is usually
			// quiet, and a per-tick line would be noise.
			if generated+failed > 0 {
				log.Printf("ledger-api-go: report sweep: generated=%d failed=%d", generated, failed)
			}
		})
		log.Printf("ledger-api-go: report sweep worker started (interval %v)", interval)
		defer sweeper.Stop()
	}

	// Optional background freeze-expiry sweeper: releases timed account
	// freezes whose auto-unfreeze deadline has passed (see
	// ledger.FreezeUntil), so operators don't need write traffic to lift
	// an expired freeze. It shares the server context, so SIGINT/SIGTERM
	// stops it. The per-tick released count is already counted by the
	// ledger itself (FrozenAutoReleases, surfaced on GET /metrics), so
	// the callback only exists for observability symmetry with the other
	// sweepers. Unset or invalid means disabled.
	if interval, ok := freezeSweepInterval(); ok {
		sweeper := ledger.StartFreezeSweeper(ctx, srv.ledger, interval, func(int) {})
		log.Printf("ledger-api-go: freeze sweep worker started (interval %v)", interval)
		defer sweeper.Stop()
	}

	// Optional background balance-health watchdog: periodically
	// re-checks the invariants the write path promises (negative
	// balances on protected accounts, accounting-equation spot check,
	// chain/version continuity) and reports breaks as structured audit
	// events plus ledger_balance_health_violations_total. It shares the
	// server context, so SIGINT/SIGTERM stops it. Unset or invalid means
	// disabled.
	if interval, ok := healthWatchInterval(); ok {
		hw := ledger.StartHealthWatch(ctx, srv.ledger, interval, func(vs []ledger.HealthViolation) {
			log.Printf("ledger-api-go: balance health watchdog: %d violation(s)", len(vs))
		})
		log.Printf("ledger-api-go: balance health watchdog started (interval %v)", interval)
		defer hw.Stop()
	}

	// Optional background FX-rate expiry watch: periodically scans the
	// rate table (LG-27/LG-34) and edge-triggers fx_rate_stale /
	// fx_rate_expired audit events plus ledger_fx_rate_stale_total /
	// ledger_fx_rate_expired_total, so operators learn a rate is about
	// to (or has) expire before transfers start failing with
	// ErrFXRateExpired. It shares the server context, so SIGINT/SIGTERM
	// stops it. Unset or invalid interval means disabled; unset or
	// invalid warning threshold disables stale warnings only (expiry
	// alerts stay active).
	if interval, threshold, ok := fxRateWatchConfig(); ok {
		fw := ledger.StartFXRateWatch(ctx, srv.ledger, interval, threshold, func(as []ledger.FXRateAlert) {
			log.Printf("ledger-api-go: fx rate watch: %d alert(s)", len(as))
		})
		log.Printf("ledger-api-go: fx rate watch started (interval %v, warning threshold %v)", interval, threshold)
		defer fw.Stop()
	}

	// Optional background FX-rate triangular-consistency watch (LG-76):
	// periodically scans the rate table for directed 3-cycles whose
	// exact product deviates from 1 by more than the opt-in
	// basis-point threshold, and edge-triggers fx_rate_inconsistent
	// audit events plus ledger_fx_rate_inconsistency_total. Advisory
	// only — conversions are never blocked. It shares the server
	// context, so SIGINT/SIGTERM stops it. Unset or invalid interval
	// disables the watcher; unset or invalid threshold disables the
	// check (the threshold is opt-in: without it microscopic feed
	// wobble would page).
	if interval, thresholdBps, ok := fxConsistencyWatchConfig(); ok {
		cw := ledger.StartFXConsistencyWatch(ctx, srv.ledger, interval, thresholdBps, func(as []ledger.FXRateInconsistency) {
			log.Printf("ledger-api-go: fx consistency watch: %d alert(s)", len(as))
		})
		log.Printf("ledger-api-go: fx consistency watch started (interval %v, threshold %d bps)", interval, thresholdBps)
		defer cw.Stop()
	}

	// Optional periodic snapshot backup worker: exports full/incremental
	// disaster-recovery snapshots to a directory on a ticker, with
	// retention. It shares the server context, so SIGINT/SIGTERM stops
	// it; every tick is counted by ledger_snapshot_backups_total /
	// ledger_snapshot_backups_failed_total. Unset dir means disabled.
	if cfg, ok := snapshotBackupConfig(); ok {
		bk := ledger.StartSnapshotBackup(ctx, srv.ledger, cfg, func(_ string, ok bool) {
			if ok {
				srv.metrics.SnapshotBackupsTotal.Add(1)
			} else {
				srv.metrics.SnapshotBackupsFailed.Add(1)
			}
		})
		log.Printf("ledger-api-go: snapshot backup worker started (dir %s, interval %v)", cfg.Dir, cfg.Interval)
		defer bk.Stop()
	}
	// GET /snapshots/diff resolves snapshot versions from the backup
	// directory directly (it needs no running worker — snapshots may
	// have been placed there by any means). Unset means the diff
	// endpoint answers 404 fail-closed.
	srv.snapshotBackupDir = os.Getenv("LEDGER_SNAPSHOT_BACKUP_DIR")
	// The diff endpoint decrypts LG-77 encrypted backups with the same
	// key ring the backup worker uses.
	srv.backupKeys = backupEncryptionConfig().Keys

	// Optional background balance-history recorder: samples every
	// (account, currency) balance on the interval and appends the rows to
	// append-only per-day JSONL files in the configured directory,
	// pruned to the configured retention window. It shares the server
	// context, so SIGINT/SIGTERM stops it, and every successful tick is
	// counted by ledger_balance_history_snapshots_total; failed ticks are
	// logged and retried next tick. Unset dir or invalid interval means
	// disabled — the series is purely additive, so nothing else changes.
	if cfg, ok := balanceHistoryConfig(); ok {
		rec := ledger.StartBalanceHistoryRecorder(ctx, srv.ledger, cfg, func(points int, err error) {
			if err != nil {
				log.Printf("ledger-api-go: balance history tick failed: %v", err)
				return
			}
			srv.metrics.BalanceHistorySnapshots.Add(1)
		})
		srv.balanceHistoryDir = cfg.Dir
		log.Printf("ledger-api-go: balance history worker started (dir %s, interval %v)", cfg.Dir, cfg.Interval)
		defer rec.Stop()
	}

	// Optional audit-sink webhook forwarder: streams outbox domain events
	// to an external endpoint as HMAC-signed JSON webhooks (see
	// ledger.StartAuditSink). LEDGER_AUDIT_SINK_URL enables it;
	// LEDGER_AUDIT_SINK_SECRET is required alongside — a URL without a
	// secret fails fast, because unsigned webhooks can be forged. An
	// unset URL means disabled. Like the other background workers it
	// stays on the default tenant's ledger: per-tenant forwarding is out
	// of scope.
	if sinkURL := os.Getenv("LEDGER_AUDIT_SINK_URL"); sinkURL != "" {
		sinkSecret := os.Getenv("LEDGER_AUDIT_SINK_SECRET")
		if sinkSecret == "" {
			log.Fatalf("ledger-api-go: LEDGER_AUDIT_SINK_URL is set but LEDGER_AUDIT_SINK_SECRET is empty: refusing to forward unsigned webhooks")
		}
		sink, err := ledger.StartAuditSink(ctx, srv.ledger, ledger.SinkConfig{
			URL:    sinkURL,
			Secret: []byte(sinkSecret),
		})
		if err != nil {
			log.Fatalf("ledger-api-go: audit sink: %v", err)
		}
		srv.auditSink = sink
		log.Printf("ledger-api-go: audit sink started (url %s)", sinkURL)
		defer sink.Stop()
	}

	timeout := shutdownTimeout()
	log.Printf("ledger-api-go listening on %s (shutdown timeout %v)", ln.Addr(), timeout)
	if err := runServer(ctx, ln, srv.handler(), timeout); err != nil {
		log.Fatalf("ledger-api-go: %v", err)
	}
	log.Print("ledger-api-go shut down cleanly")
}

// auditMaxBytes reads LEDGER_AUDIT_MAX_BYTES (a byte count, e.g.
// "104857600") for the audit-log file size cap. Unset means the default;
// invalid or non-positive values fall back to the default with a log
// line.
func auditMaxBytes() int64 {
	raw := os.Getenv("LEDGER_AUDIT_MAX_BYTES")
	if raw == "" {
		return ledger.DefaultAuditMaxBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_AUDIT_MAX_BYTES %q, using %d", raw, ledger.DefaultAuditMaxBytes)
		return ledger.DefaultAuditMaxBytes
	}
	return n
}

// auditSigningGrace reads LEDGER_AUDIT_SIGNING_GRACE (a Go duration
// string, e.g. "24h") for the audit-log signature verification grace
// window after a signing key retires (see ledger/audit_sign.go). Unset
// means the default; invalid or negative values fall back to the default
// with a log line.
func auditSigningGrace() time.Duration {
	raw := os.Getenv("LEDGER_AUDIT_SIGNING_GRACE")
	if raw == "" {
		return ledger.DefaultAuditSigningGrace
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_AUDIT_SIGNING_GRACE %q, using %v", raw, ledger.DefaultAuditSigningGrace)
		return ledger.DefaultAuditSigningGrace
	}
	return d
}

// scheduleSweepInterval reads LEDGER_SCHEDULE_SWEEP_INTERVAL (a Go
// duration string, e.g. "1m") for the background transfer-schedule
// worker. Unset or invalid values mean the worker is disabled; a
// non-positive duration is invalid and falls back to disabled with a log
// line.
func scheduleSweepInterval() (time.Duration, bool) {
	raw := os.Getenv("LEDGER_SCHEDULE_SWEEP_INTERVAL")
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_SCHEDULE_SWEEP_INTERVAL %q, schedule sweep worker disabled", raw)
		return 0, false
	}
	return d, true
}

// reportSweepInterval reads LEDGER_REPORT_SWEEP_INTERVAL (a Go
// duration string, e.g. "1h") for the background report-schedule
// worker. Unset or invalid values mean the worker is disabled; a
// non-positive duration is invalid and falls back to disabled with a log
// line.
func reportSweepInterval() (time.Duration, bool) {
	raw := os.Getenv("LEDGER_REPORT_SWEEP_INTERVAL")
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_REPORT_SWEEP_INTERVAL %q, report sweep worker disabled", raw)
		return 0, false
	}
	return d, true
}

// tsaMetricsAuthority wraps a ledger.TimestampAuthority to count TSA
// outcomes in the server metrics. Failures are additionally recorded
// as `tsa_anchor_failed` audit events by the worker itself (the
// degraded alert); the counter here feeds /metrics scraping.
type tsaMetricsAuthority struct {
	inner   ledger.TimestampAuthority
	metrics *Metrics
}

func (a *tsaMetricsAuthority) Timestamp(ctx context.Context, digest [32]byte) (ledger.TimestampToken, error) {
	tok, err := a.inner.Timestamp(ctx, digest)
	if err != nil {
		a.metrics.TSAAnchorFailuresTotal.Add(1)
	}
	return tok, err
}

// tsaAnchorInterval reads LEDGER_TSA_INTERVAL (a Go duration string,
// e.g. "1h") for the background RFC 3161 anchoring worker. Unset or
// invalid values mean the worker is disabled; a non-positive duration
// is invalid and falls back to disabled with a log line.
func tsaAnchorInterval() (time.Duration, bool) {
	raw := os.Getenv("LEDGER_TSA_INTERVAL")
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_TSA_INTERVAL %q, TSA anchoring worker disabled", raw)
		return 0, false
	}
	return d, true
}

// holdSweepInterval reads LEDGER_HOLD_SWEEP_INTERVAL (a Go duration string,
// e.g. "30s") for the background hold-expiry worker. Unset or invalid
// values mean the worker is disabled; a non-positive duration is invalid
// and falls back to disabled with a log line.
func holdSweepInterval() (time.Duration, bool) {
	raw := os.Getenv("LEDGER_HOLD_SWEEP_INTERVAL")
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_HOLD_SWEEP_INTERVAL %q, hold sweep worker disabled", raw)
		return 0, false
	}
	return d, true
}

// freezeSweepInterval reads LEDGER_FREEZE_SWEEP_INTERVAL (a Go duration
// string, e.g. "1m") for the background freeze-expiry worker that
// releases timed account freezes whose auto-unfreeze deadline has passed
// (see ledger.StartFreezeSweeper). Unset or invalid values mean the
// worker is disabled; a non-positive duration is invalid and falls back
// to disabled with a log line.
func freezeSweepInterval() (time.Duration, bool) {
	raw := os.Getenv("LEDGER_FREEZE_SWEEP_INTERVAL")
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_FREEZE_SWEEP_INTERVAL %q, freeze sweep worker disabled", raw)
		return 0, false
	}
	return d, true
}

// healthWatchInterval reads LEDGER_HEALTH_WATCH_INTERVAL (a Go duration
// string, e.g. "5m") for the background balance-health watchdog (see
// ledger.StartHealthWatch). Unset or invalid values mean the worker is
// disabled; a non-positive duration is invalid and falls back to disabled
// with a log line.
func healthWatchInterval() (time.Duration, bool) {
	raw := os.Getenv("LEDGER_HEALTH_WATCH_INTERVAL")
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_HEALTH_WATCH_INTERVAL %q, balance health watchdog disabled", raw)
		return 0, false
	}
	return d, true
}

// fxRateWatchConfig reads LEDGER_FX_RATE_WATCH_INTERVAL (a Go duration
// string, e.g. "5m") and LEDGER_FX_RATE_WARNING_THRESHOLD (a Go duration
// string, e.g. "72h") for the background FX-rate expiry watch (see
// ledger.StartFXRateWatch). Unset or invalid interval means the watcher
// is disabled. The warning threshold is opt-in: unset or invalid falls
// back to zero with a log line, which disables stale warnings while
// expiry alerts stay active.
func fxRateWatchConfig() (interval, warningThreshold time.Duration, ok bool) {
	raw := os.Getenv("LEDGER_FX_RATE_WATCH_INTERVAL")
	if raw == "" {
		return 0, 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_FX_RATE_WATCH_INTERVAL %q, fx rate watch disabled", raw)
		return 0, 0, false
	}
	thresholdRaw := os.Getenv("LEDGER_FX_RATE_WARNING_THRESHOLD")
	if thresholdRaw != "" {
		t, err := time.ParseDuration(thresholdRaw)
		if err != nil || t <= 0 {
			log.Printf("ledger-api-go: ignoring invalid LEDGER_FX_RATE_WARNING_THRESHOLD %q, stale warnings disabled", thresholdRaw)
		} else {
			warningThreshold = t
		}
	}
	return d, warningThreshold, true
}

// fxConsistencyWatchConfig reads LEDGER_FX_CONSISTENCY_INTERVAL (a Go
// duration string, e.g. "5m") for the background FX triangular-
// consistency watcher (LG-76) and LEDGER_FX_CONSISTENCY_THRESHOLD_BPS
// (an integer basis-point count) for its alert threshold. Unset or
// invalid interval means the watcher is disabled; unset or invalid
// threshold disables the check entirely — the threshold is opt-in, so
// microscopic feed wobble never pages by default.
func fxConsistencyWatchConfig() (interval time.Duration, thresholdBps int64, ok bool) {
	raw := os.Getenv("LEDGER_FX_CONSISTENCY_INTERVAL")
	if raw == "" {
		return 0, 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_FX_CONSISTENCY_INTERVAL %q, fx consistency watch disabled", raw)
		return 0, 0, false
	}
	bpsRaw := os.Getenv("LEDGER_FX_CONSISTENCY_THRESHOLD_BPS")
	bps, err := strconv.ParseInt(bpsRaw, 10, 64)
	if err != nil || bps <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_FX_CONSISTENCY_THRESHOLD_BPS %q, fx consistency check disabled", bpsRaw)
		return 0, 0, false
	}
	return d, bps, true
}

// reviewSweepInterval reads LEDGER_REVIEW_SWEEP_INTERVAL (a Go duration
// string, e.g. "5m") for the background transfer-review expiry worker.
// Unset or invalid values mean the worker is disabled; a non-positive
// duration is invalid and falls back to disabled with a log line.
func reviewSweepInterval() (time.Duration, bool) {
	raw := os.Getenv("LEDGER_REVIEW_SWEEP_INTERVAL")
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_REVIEW_SWEEP_INTERVAL %q, review sweep worker disabled", raw)
		return 0, false
	}
	return d, true
}

// snapshotBackupConfig reads the LEDGER_SNAPSHOT_BACKUP_* env vars for the
// periodic snapshot backup worker (LG-39):
//
//   - LEDGER_SNAPSHOT_BACKUP_DIR: backup directory. Unset means the
//     worker is disabled.
//   - LEDGER_SNAPSHOT_BACKUP_INTERVAL: Go duration string (e.g. "1h").
//     Unset or invalid means disabled.
//   - LEDGER_SNAPSHOT_BACKUP_KEEP_FULL: full backups to retain
//     (default 7); invalid or non-positive falls back to the default.
//   - LEDGER_SNAPSHOT_BACKUP_KEEP_INCR: incremental backups to retain
//     (default 24); invalid or non-positive falls back to the default.
//   - LEDGER_SNAPSHOT_BACKUP_FULL_EVERY: full snapshot every Nth backup
//     (default 6); invalid or non-positive falls back to the default.
//   - LEDGER_BACKUP_ENCRYPTION_KEY (LG-77): opt-in AES-256-GCM at-rest
//     encryption for backup files — 64 hex chars for a single key, or
//     "kid:hex,kid:hex,..." for a rotation ring (first entry active).
//     Invalid values are rejected with a log line and treated as
//     unconfigured: the worker then writes plaintext (fail-open, with
//     a one-time audit warning) unless
//     LEDGER_SNAPSHOT_BACKUP_ENCRYPT_FAIL_CLOSED is set.
//   - LEDGER_SNAPSHOT_BACKUP_ENCRYPT_FAIL_CLOSED: "1"/"true" makes
//     backup ticks fail instead of writing plaintext when no
//     encryption key is configured.
func snapshotBackupConfig() (ledger.SnapshotBackupConfig, bool) {
	dir := os.Getenv("LEDGER_SNAPSHOT_BACKUP_DIR")
	if dir == "" {
		return ledger.SnapshotBackupConfig{}, false
	}
	raw := os.Getenv("LEDGER_SNAPSHOT_BACKUP_INTERVAL")
	interval, err := time.ParseDuration(raw)
	if err != nil || interval <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_SNAPSHOT_BACKUP_INTERVAL %q, snapshot backup worker disabled", raw)
		return ledger.SnapshotBackupConfig{}, false
	}
	cfg := ledger.SnapshotBackupConfig{
		Dir:             dir,
		Interval:        interval,
		KeepFull:        snapshotBackupKeep("LEDGER_SNAPSHOT_BACKUP_KEEP_FULL", ledger.DefaultSnapshotBackupKeepFull),
		KeepIncremental: snapshotBackupKeep("LEDGER_SNAPSHOT_BACKUP_KEEP_INCR", ledger.DefaultSnapshotBackupKeepIncremental),
		FullEvery:       snapshotBackupKeep("LEDGER_SNAPSHOT_BACKUP_FULL_EVERY", ledger.DefaultSnapshotBackupFullEvery),
		Encryption:      backupEncryptionConfig(),
	}
	return cfg, true
}

// backupEncryptionConfig parses the LG-77 encryption env vars:
// LEDGER_BACKUP_ENCRYPTION_KEY (the key or rotation ring) and
// LEDGER_SNAPSHOT_BACKUP_ENCRYPT_FAIL_CLOSED. A malformed key is a
// log line, not a crash: it parses as unconfigured, and the fail-open
// / fail-closed policy decides what the worker does about it.
func backupEncryptionConfig() ledger.BackupEncryptionConfig {
	var cfg ledger.BackupEncryptionConfig
	raw := os.Getenv("LEDGER_BACKUP_ENCRYPTION_KEY")
	parsed, err := ledger.ParseBackupEncryptionKeys(raw)
	if err != nil {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_BACKUP_ENCRYPTION_KEY: %v", err)
	} else {
		cfg = parsed
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LEDGER_SNAPSHOT_BACKUP_ENCRYPT_FAIL_CLOSED"))) {
	case "1", "true", "yes":
		cfg.FailClosed = true
	}
	return cfg
}

// balanceHistoryConfig reads LEDGER_BALANCE_HISTORY_DIR (the directory
// the recorder appends per-day balance JSONL to) and
// LEDGER_BALANCE_HISTORY_INTERVAL (a Go duration string, e.g. "5m") for
// the background balance-history recorder. Unset dir means disabled; an
// unset or invalid interval means disabled with a log line — the journal
// itself is the source of truth, so history collection is purely
// additive and never required for correctness.
func balanceHistoryConfig() (ledger.BalanceHistoryConfig, bool) {
	dir := os.Getenv("LEDGER_BALANCE_HISTORY_DIR")
	if dir == "" {
		return ledger.BalanceHistoryConfig{}, false
	}
	raw := os.Getenv("LEDGER_BALANCE_HISTORY_INTERVAL")
	interval, err := time.ParseDuration(raw)
	if err != nil || interval <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_BALANCE_HISTORY_INTERVAL %q, balance history worker disabled", raw)
		return ledger.BalanceHistoryConfig{}, false
	}
	return ledger.BalanceHistoryConfig{
		Dir:      dir,
		Interval: interval,
		MaxAgeMs: balanceHistoryMaxAgeMs(),
	}, true
}

// balanceHistoryMaxAgeMs reads LEDGER_BALANCE_HISTORY_MAX_AGE_MS (a
// millisecond count) for the recorder's retention window. Unset means
// the default; invalid or non-positive values fall back to the default
// with a log line.
func balanceHistoryMaxAgeMs() int64 {
	raw := os.Getenv("LEDGER_BALANCE_HISTORY_MAX_AGE_MS")
	if raw == "" {
		return ledger.DefaultBalanceHistoryMaxAgeMs
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		log.Printf("ledger-api-go: ignoring invalid LEDGER_BALANCE_HISTORY_MAX_AGE_MS %q, using %d", raw, ledger.DefaultBalanceHistoryMaxAgeMs)
		return ledger.DefaultBalanceHistoryMaxAgeMs
	}
	return n
}

// snapshotBackupKeep reads an integer env var for the backup worker;
// invalid or non-positive values fall back to def with a log line.
func snapshotBackupKeep(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("ledger-api-go: ignoring invalid %s %q, using %d", name, raw, def)
		return def
	}
	return n
}

// shutdownTimeout reads SHUTDOWN_TIMEOUT (a Go duration string, e.g. "15s").
// It defaults to 10s; invalid or non-positive values fall back to the default
// with a log line.
func shutdownTimeout() time.Duration {
	const def = 10 * time.Second
	raw := os.Getenv("SHUTDOWN_TIMEOUT")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ledger-api-go: ignoring invalid SHUTDOWN_TIMEOUT %q, using %v", raw, def)
		return def
	}
	return d
}

// runServer serves handler on ln until ctx is cancelled, then performs a
// graceful shutdown: it stops accepting new connections and waits for
// in-flight requests to finish, up to shutdownTimeout. It returns nil on a
// clean shutdown, or the underlying server error.
func runServer(ctx context.Context, ln net.Listener, handler http.Handler, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second, // bound slowloris-style header drips
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		// Serve reports ErrServerClosed after a graceful Shutdown; that is
		// the expected path, not an error.
		if err := <-errCh; err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case err := <-errCh:
		return err
	}
}
