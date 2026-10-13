package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// postJSONWithActor is postJSON with an x-ledger-actor header.
func postJSONWithActor(t *testing.T, url, payload, actor string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if actor != "" {
		req.Header.Set("x-ledger-actor", actor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, body
}

func TestXLedgerActorHeader(t *testing.T) {
	dir := t.TempDir()
	al, err := ledger.NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := ledger.New(ledger.WithAuditLog(al))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// With the header: the operator identity is attributed.
	if code, _ := postJSONWithActor(t, srv.URL+"/accounts/cash/freeze", "", "ops-alan"); code != http.StatusOK {
		t.Fatalf("freeze with actor: status=%d, want 200", code)
	}
	// Without the header: the default "system" is attributed.
	if code, _ := postJSONWithActor(t, srv.URL+"/accounts/vault/freeze", "", ""); code != http.StatusOK {
		t.Fatalf("freeze without actor: status=%d, want 200", code)
	}
	// Overlong values are rejected before any audit write. (Control
	// characters never reach the server: Go's HTTP client rejects them
	// at request construction; the server-side check in actorCtx is
	// defense in depth for hand-crafted raw requests.)
	long := strings.Repeat("x", 129)
	if code, _ := postJSONWithActor(t, srv.URL+"/accounts/cash/unfreeze", "", long); code != http.StatusBadRequest {
		t.Fatalf("overlong actor: status=%d, want 400", code)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ledger.ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d", corrupt)
	}
	actors := map[string]string{}
	for _, ev := range events {
		if ev.Op == "freeze" {
			actors[ev.TraceID] = ev.Actor
		}
	}
	if actors["cash"] != "ops-alan" {
		t.Fatalf("freeze(cash) actor = %q, want ops-alan", actors["cash"])
	}
	if actors["vault"] != "system" {
		t.Fatalf("freeze(vault) actor = %q, want system", actors["vault"])
	}
}

func TestXLedgerActorApproveReview(t *testing.T) {
	dir := t.TempDir()
	al, err := ledger.NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := ledger.New(ledger.WithAuditLog(al), ledger.WithReviewThreshold(10000))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// Seed payer via the HTTP surface.
	if code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"equity","amount_cents":100000}`); code != http.StatusCreated {
		t.Fatalf("seed: status=%d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx1","from_account":"payer","to_account":"payee","amount_cents":50000}`); code != http.StatusCreated {
		t.Fatalf("transfer: status=%d", code)
	}
	if code, _ := postJSONWithActor(t, srv.URL+"/transfers/tx1/approve",
		`{"reviewer":"risk-lead"}`, "risk-lead"); code != http.StatusOK {
		t.Fatalf("approve: status=%d, want 200", code)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, _, err := ledger.ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Op == "transfer_review_approved" {
			found = true
			if ev.Actor != "risk-lead" {
				t.Fatalf("approved actor = %q, want risk-lead", ev.Actor)
			}
		}
	}
	if !found {
		t.Fatalf("no transfer_review_approved event")
	}
}
