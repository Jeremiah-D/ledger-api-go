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
// with the originally posted entry; invalid entries return 400; a post
// through a frozen account returns 403; a post that would overdraw an
// overdraft-protected account returns 422.
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

// handleBalance implements GET /accounts/{id}/balance.
func (s *server) handleBalance(w http.ResponseWriter, r *http.Request) {
	s.metrics.BalanceQueries.Add(1)
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id required"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":       id,
		"balance_cents": s.ledger.Balance(ledger.AccountID(id)),
		"frozen":        s.ledger.IsFrozen(ledger.AccountID(id)),
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

// handleReconcile implements POST /reconcile, the operator-facing end-of-day
// reconciliation job. It runs a full read-only scan of the live ledger — the
// accounting equation, per-account trial balances, idempotency-key health,
// and audit-chain integrity — and returns the report as the response body.
// A reconcile run that reports an unhealthy ledger is still a successful
// request, so the status is always 200: the findings live inside the
// report. The indented JSON body pipes straight into a dated archive:
//
//	curl -s -X POST localhost:8080/reconcile | tee reconcile-$(date +%F).json
func (s *server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	s.metrics.ReconcileRuns.Add(1)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	report := s.ledger.Reconcile(time.Now())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := report.WriteJSON(w); err != nil {
		log.Printf("ledger-api-go: POST /reconcile encode error: %v", err)
	}
}

func newRouter(l *ledger.Ledger) http.Handler {
	s := &server{ledger: l, metrics: &Metrics{}, maxBodyBytes: maxRequestBodyBytes()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /entries", s.handleCreateEntry)
	mux.HandleFunc("POST /reconcile", s.handleReconcile)
	mux.HandleFunc("POST /accounts/{id}/freeze", s.handleFreezeAccount)
	mux.HandleFunc("POST /accounts/{id}/unfreeze", s.handleUnfreezeAccount)
	mux.HandleFunc("GET /entries", s.handleListEntries)
	mux.HandleFunc("GET /entries/verify", s.handleVerifyEntries)
	mux.HandleFunc("GET /accounts/{id}/balance", s.handleBalance)
	mux.HandleFunc("GET /accounts/{id}/entries", s.handleListAccountEntries)
	mux.HandleFunc("GET /accounts/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /accounts/{id}/trial-balance", s.handleTrialBalance)
	mux.HandleFunc("GET /metrics", s.metrics.handleMetrics)
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

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("ledger-api-go: listen %s: %v", addr, err)
	}

	// SIGINT/SIGTERM cancel the context; runServer then drains in-flight
	// requests instead of dropping them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	timeout := shutdownTimeout()
	log.Printf("ledger-api-go listening on %s (shutdown timeout %v)", ln.Addr(), timeout)
	if err := runServer(ctx, ln, newRouter(ledger.New(opts...)), timeout); err != nil {
		log.Fatalf("ledger-api-go: %v", err)
	}
	log.Print("ledger-api-go shut down cleanly")
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
