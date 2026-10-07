// Command ledger-api-go serves a minimal double-entry ledger over HTTP.
// It is intentionally thin: all bookkeeping logic lives in the ledger
// package; this file only assembles the HTTP transport.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

type server struct {
	ledger  *ledger.Ledger
	metrics *Metrics
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
// with the originally posted entry; invalid entries return 400.
func (s *server) handleCreateEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	defer r.Body.Close()

	// Every POST attempt is counted; replays are counted separately below.
	s.metrics.PostsTotal.Add(1)

	var req createEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
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
	})
}

func newRouter(l *ledger.Ledger) http.Handler {
	s := &server{ledger: l, metrics: &Metrics{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /entries", s.handleCreateEntry)
	mux.HandleFunc("GET /entries", s.handleListEntries)
	mux.HandleFunc("GET /accounts/{id}/balance", s.handleBalance)
	mux.HandleFunc("GET /accounts/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /metrics", s.metrics.handleMetrics)
	return mux
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
	q := r.URL.Query()

	since := time.Time{}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid since timestamp (want RFC3339)"})
			return
		}
		since = t
	}
	until := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid until timestamp (want RFC3339)"})
			return
		}
		until = t
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid limit (want integer)"})
			return
		}
		limit = n
	}

	page, next, err := s.ledger.ListEntries(since, until, q.Get("cursor"), limit)
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
