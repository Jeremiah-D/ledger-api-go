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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

type server struct {
	ledger       *ledger.Ledger
	metrics      *Metrics
	maxBodyBytes int64
}

type createEntryRequest struct {
	DebitAccount   ledger.AccountID `json:"debit_account"`
	CreditAccount  ledger.AccountID `json:"credit_account"`
	AmountCents    int64            `json:"amount_cents"`
	Currency       string           `json:"currency"`
	IdempotencyKey string           `json:"idempotency_key"`
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
		CreatedAt:      time.Now(),
	}

	posted, duplicate, err := s.ledger.Post(entry)
	if err != nil {
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
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
		if errors.Is(err, ledger.ErrInvalidCurrency) {
			s.metrics.CurrencyRejections.Add(1)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if duplicate {
		s.metrics.IdempotencyHits.Add(1)
		writeJSON(w, http.StatusOK, posted)
		return
	}
	writeJSON(w, http.StatusCreated, posted)
}

type createTransferRequest struct {
	TransferID     string           `json:"transfer_id"`
	FromAccount    ledger.AccountID `json:"from_account"`
	ToAccount      ledger.AccountID `json:"to_account"`
	AmountCents    int64            `json:"amount_cents"`
	Currency       string           `json:"currency"`
	ToCurrency     string           `json:"to_currency"`
	FXAccount      ledger.AccountID `json:"fx_account"`
	FeeCents       int64            `json:"fee_cents"`
	FeeAccount     ledger.AccountID `json:"fee_account"`
	SkipFee        bool             `json:"skip_fee"`
	IdempotencyKey string           `json:"idempotency_key"`
}

// handleCreateTransfer implements POST /transfers, the payment-domain view
// of a posting: the caller names the payer (from_account) and the payee
// (to_account) and the ledger books the double-entry pair atomically —
// either every leg lands or nothing does. An optional fee leg
// (fee_cents + fee_account, or the server-wide LEDGER_TRANSFER_FEE policy
// unless skip_fee is set) charges the payer on top of the amount and books
// it to the fee account as a second entry; the receipt's fee_cents reports
// what was booked.
//
// The server generates transfer_id when the client omits it. A first-time
// transfer returns 201 with the receipt; a duplicate idempotency key
// returns 200 with the originally posted receipt; invalid transfers return
// 400; a transfer through a frozen account returns 403; a transfer that
// would overdraw an overdraft-protected payer returns 422, as does a
// transfer that would take the payer's daily outflow (amount + fee) above
// its configured limit. A cross-currency transfer (to_currency set and
// different from currency) converts at the configured rate for the pair
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
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      time.Now(),
	}
	if req.ToCurrency != "" {
		s.metrics.FXTransfersTotal.Add(1)
	}

	receipt, err := s.ledger.PostTransfer(transfer)
	if err != nil {
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
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
		if errors.Is(err, ledger.ErrFXAccountNotConfigured) || errors.Is(err, ledger.ErrInvalidFXAccount) {
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
	writeJSON(w, http.StatusCreated, receipt)
}

type batchEntryRequest struct {
	EntryID        string           `json:"entry_id"`
	DebitAccount   ledger.AccountID `json:"debit_account"`
	CreditAccount  ledger.AccountID `json:"credit_account"`
	AmountCents    int64            `json:"amount_cents"`
	Currency       string           `json:"currency"`
	IdempotencyKey string           `json:"idempotency_key"`
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
			CreatedAt:      time.Now(),
		})
	}
	batch := ledger.Batch{
		ID:             id,
		Entries:        entries,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      time.Now(),
	}

	receipt, err := s.ledger.PostBatch(batch)
	if err != nil {
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
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
	receipt, err := s.ledger.PostSweep(ledger.Sweep{
		ID:             id,
		From:           req.FromAccounts,
		To:             req.ToAccount,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      time.Now(),
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
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
	receipt, err := s.ledger.PostMerge(ledger.Merge{
		ID:             id,
		From:           req.FromAccount,
		To:             req.ToAccount,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      time.Now(),
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
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

// decodeJSONBody decodes a JSON request body with the same transport
// contract as POST /entries: bounded body, strict decoding (unknown
// fields fail fast), no trailing garbage. It returns false after writing
// the error response when decoding fails. Callers must check the HTTP
// method before calling it; it always closes the body.
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
	held, duplicate, err := s.ledger.Hold(ledger.Hold{
		ID:             id,
		Account:        req.Account,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		ExpiresAt:      req.ExpiresAt,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      time.Now(),
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
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
	CaptureID      string           `json:"capture_id"`
	ToAccount      ledger.AccountID `json:"to_account"`
	AmountCents    int64            `json:"amount_cents"`
	IdempotencyKey string           `json:"idempotency_key"`
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
	receipt, err := s.ledger.Capture(ledger.Capture{
		ID:             captureID,
		HoldID:         id,
		To:             req.ToAccount,
		AmountCents:    req.AmountCents,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      time.Now(),
	})
	if err != nil {
		if errors.Is(err, ledger.ErrHoldNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountFrozen) {
			s.metrics.FrozenRejections.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ledger.ErrAccountOverdraft) {
			s.metrics.OverdraftRejections.Add(1)
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
	h, err := s.ledger.Release(id)
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
	writeJSON(w, http.StatusOK, map[string]any{"expired": s.ledger.ExpireHolds()})
}

// handleBalance implements GET /accounts/{id}/balance.
//
// available_cents is the account's spendable funds: net balance minus
// active authorization holds (see POST /holds). A hold reserves funds
// without moving them, so balance_cents keeps reporting the journaled
// net while available_cents reports what can still be authorized.
func (s *server) handleBalance(w http.ResponseWriter, r *http.Request) {
	s.metrics.BalanceQueries.Add(1)
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":         id,
		"balance_cents":   s.ledger.Balance(ledger.AccountID(id)),
		"available_cents": s.ledger.Available(ledger.AccountID(id)),
		"frozen":          s.ledger.IsFrozen(ledger.AccountID(id)),
	})
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
	balance, version := s.ledger.Snapshot(ledger.AccountID(id))
	writeJSON(w, http.StatusOK, map[string]any{
		"account":       id,
		"balance_cents": balance,
		"version":       version,
		"frozen":        s.ledger.IsFrozen(ledger.AccountID(id)),
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
	version, err := strconv.ParseUint(r.URL.Query().Get("version"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version query parameter is required as a non-negative integer"})
		return
	}
	currency := r.URL.Query().Get("currency")
	if currency == "" {
		currency = ledger.DefaultCurrency
	}
	balance, err := s.ledger.BalanceAt(ledger.AccountID(id), currency, version)
	if err != nil {
		if errors.Is(err, ledger.ErrVersionInFuture) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":       id,
		"currency":      currency,
		"version":       version,
		"balance_cents": balance,
	})
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
	writeJSON(w, http.StatusOK, s.ledger.TrialBalance(ledger.AccountID(id)))
}

// setParentRequest is the body of POST /accounts/{id}/parent. An empty
// parent clears the assignment.
type setParentRequest struct {
	Parent ledger.AccountID `json:"parent"`
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

	if err := s.ledger.SetParent(ledger.AccountID(id), req.Parent); err != nil {
		if errors.Is(err, ledger.ErrParentCycle) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	parent, _ := s.ledger.Parent(ledger.AccountID(id))
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
	writeJSON(w, http.StatusOK, s.ledger.Rollup(ledger.AccountID(id)))
}

// handleVerifyEntries implements GET /entries/verify. It recomputes the
// ledger's tamper-evident audit chain and reports whether it is intact:
// 200 {"ok":true,"links":N,"head":"<hex>"} on success. A broken chain is an
// operator-level integrity incident, not a client error, so verification
// failure returns 500 {"ok":false,"error":"..."}.
func (s *server) handleVerifyEntries(w http.ResponseWriter, r *http.Request) {
	s.metrics.VerifyRequests.Add(1)
	if err := s.ledger.VerifyChain(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	head, links := s.ledger.ChainHead()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "links": links, "head": head})
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
	dir, ok := s.ledger.AuditDir()
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

// handleFreezeAccount implements POST /accounts/{id}/freeze: the
// operator-facing risk-control stop. A frozen account rejects every new
// POST /entries that names it as either leg with 403, while balance,
// snapshot, trial-balance, entries, chain verification, and the
// end-of-day reconcile keep working. Freezing is idempotent and does not
// bump the ledger version (balances are unchanged by a freeze).
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
	s.ledger.Freeze(ledger.AccountID(id))
	writeJSON(w, http.StatusOK, map[string]any{"account": id, "frozen": true})
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
	s.ledger.Unfreeze(ledger.AccountID(id))
	writeJSON(w, http.StatusOK, map[string]any{"account": id, "frozen": false})
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
	// unknown fields fail fast. An empty body means "no options" — the
	// legacy scan — so existing cron jobs keep working unchanged.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	opts := ledger.ReconcileOptions{}
	var req reconcileRequest
	switch err := dec.Decode(&req); {
	case err == io.EOF:
		// No body: legacy report, fx_applied=false.
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
		opts.BaseCurrency = req.BaseCurrency
	}

	report, err := s.ledger.ReconcileWithOptions(time.Now(), opts)
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

func newRouter(l *ledger.Ledger) http.Handler {
	return newServer(l).handler()
}

// newServer builds the server the same way newRouter does but also hands
// the caller the *server, so main can attach background workers (the hold
// sweeper) to its metrics. Tests keep using newRouter.
func newServer(l *ledger.Ledger) *server {
	return &server{ledger: l, metrics: &Metrics{}, maxBodyBytes: maxRequestBodyBytes()}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /entries", s.handleCreateEntry)
	mux.HandleFunc("POST /entries/batch", s.handleCreateBatch)
	mux.HandleFunc("POST /transfers", s.handleCreateTransfer)
	mux.HandleFunc("POST /sweeps", s.handleCreateSweep)
	mux.HandleFunc("POST /merges", s.handleCreateMerge)
	mux.HandleFunc("POST /reconcile", s.handleReconcile)
	mux.HandleFunc("POST /holds", s.handleCreateHold)
	mux.HandleFunc("POST /holds/expire", s.handleExpireHolds)
	mux.HandleFunc("POST /holds/{id}/capture", s.handleCaptureHold)
	mux.HandleFunc("POST /holds/{id}/release", s.handleReleaseHold)
	mux.HandleFunc("POST /accounts/{id}/freeze", s.handleFreezeAccount)
	mux.HandleFunc("POST /accounts/{id}/unfreeze", s.handleUnfreezeAccount)
	mux.HandleFunc("POST /accounts/{id}/parent", s.handleSetParent)
	mux.HandleFunc("GET /entries", s.handleListEntries)
	mux.HandleFunc("GET /entries/verify", s.handleVerifyEntries)
	mux.HandleFunc("GET /audit/verify", s.handleAuditVerify)
	mux.HandleFunc("GET /accounts/{id}/balance", s.handleBalance)
	mux.HandleFunc("GET /accounts/{id}/entries", s.handleListAccountEntries)
	mux.HandleFunc("GET /accounts/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /accounts/{id}/balance-at", s.handleBalanceAt)
	mux.HandleFunc("GET /accounts/{id}/trial-balance", s.handleTrialBalance)
	mux.HandleFunc("GET /accounts/{id}/rollup", s.handleRollup)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		// The audit log lives in the ledger package; sync its counters
		// into the HTTP metrics on every scrape so the exposition stays
		// meaningful without the ledger knowing about HTTP.
		if written, dropped, ok := s.ledger.AuditStats(); ok {
			s.metrics.AuditEventsTotal.Store(written)
			s.metrics.AuditDroppedTotal.Store(dropped)
		}
		s.metrics.handleMetrics(w, r)
	})
	return mux
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
	since, until, limit, err := parseEntryListQuery(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	page, next, err := s.ledger.ListAccountEntries(ledger.AccountID(id), since, until, r.URL.Query().Get("cursor"), limit)
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
// page. Malformed timestamps, cursors, or limits return 400.
func (s *server) handleListEntries(w http.ResponseWriter, r *http.Request) {
	since, until, limit, err := parseEntryListQuery(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	page, next, err := s.ledger.ListEntries(since, until, r.URL.Query().Get("cursor"), limit)
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

	// LEDGER_FX_ACCOUNT configures the ledger-wide FX clearing account,
	// the counterparty of cross-currency transfer legs (see
	// ledger.WithFXAccount). Unset means cross-currency transfers must
	// carry fx_account on each request.
	if raw := os.Getenv("LEDGER_FX_ACCOUNT"); raw != "" {
		opts = append(opts, ledger.WithFXAccount(ledger.AccountID(raw)))
		log.Printf("ledger-api-go: FX clearing account = %q", raw)
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
	if dir := os.Getenv("LEDGER_AUDIT_DIR"); dir != "" {
		al, err := ledger.NewAuditLog(dir, ledger.WithAuditLogMaxBytes(auditMaxBytes()))
		if err != nil {
			log.Fatalf("ledger-api-go: audit log: %v", err)
		}
		defer al.Close()
		opts = append(opts, ledger.WithAuditLog(al))
		log.Printf("ledger-api-go: audit log enabled (dir %s)", dir)
	}

	// SIGINT/SIGTERM cancel the context; runServer then drains in-flight
	// requests instead of dropping them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := newServer(ledger.New(opts...))

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
