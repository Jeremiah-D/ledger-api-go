// Command ledger-api-go serves a minimal double-entry ledger over HTTP.
// It is intentionally thin: all bookkeeping logic lives in the ledger
// package; this file only assembles the HTTP transport.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

type server struct {
	ledger *ledger.Ledger
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
		writeJSON(w, http.StatusOK, posted)
		return
	}
	writeJSON(w, http.StatusCreated, posted)
}

// handleBalance implements GET /accounts/{id}/balance.
func (s *server) handleBalance(w http.ResponseWriter, r *http.Request) {
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

func newRouter(l *ledger.Ledger) http.Handler {
	s := &server{ledger: l}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /entries", s.handleCreateEntry)
	mux.HandleFunc("GET /accounts/{id}/balance", s.handleBalance)
	return mux
}

func main() {
	addr := os.Getenv("PORT")
	if addr == "" {
		addr = "8080"
	}
	if !strings.HasPrefix(addr, ":") {
		addr = ":" + addr
	}

	log.Printf("ledger-api-go listening on %s", addr)
	if err := http.ListenAndServe(addr, newRouter(ledger.New())); err != nil {
		log.Fatal(err)
	}
}
