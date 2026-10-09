package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestMergeLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Fund the source account through the raw entries endpoint.
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"old-corp","credit_account":"funding","amount_cents":10000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding: status=%d, want 201", code)
	}

	// First merge: 201 with the receipt; the leg carries the merge-ID
	// prefix and the source is frozen.
	code, body := postJSON(t, srv.URL+"/merges",
		`{"merge_id":"mg-1","from_account":"old-corp","to_account":"new-corp","idempotency_key":"mk-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("merge: status=%d, want 201 (body=%v)", code, body)
	}
	if body["merge_id"] != "mg-1" || body["duplicate"] != false {
		t.Errorf("receipt = %v, want mg-1 non-duplicate", body)
	}
	if body["source_frozen"] != true {
		t.Errorf("source_frozen = %v, want true", body["source_frozen"])
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries = %v, want exactly one merge leg", body["entries"])
	}
	e := entries[0].(map[string]any)
	if e["id"] != "mg-1/USD" || e["debit_account"] != "new-corp" || e["credit_account"] != "old-corp" {
		t.Errorf("leg entry = %v, want id mg-1/USD debit new-corp credit old-corp", e)
	}

	// Replay of the same idempotency key: 200 with the original receipt,
	// even though the source is now frozen.
	code, body = postJSON(t, srv.URL+"/merges",
		`{"merge_id":"mg-1-retry","from_account":"old-corp","to_account":"new-corp","idempotency_key":"mk-1"}`)
	if code != http.StatusOK {
		t.Fatalf("replay merge: status=%d, want 200 (body=%v)", code, body)
	}
	if body["duplicate"] != true || body["merge_id"] != "mg-1" {
		t.Errorf("replay = %v, want duplicate of mg-1", body)
	}

	// A new merge from the frozen source: 403.
	code, _ = postJSON(t, srv.URL+"/merges",
		`{"merge_id":"mg-2","from_account":"old-corp","to_account":"new-corp"}`)
	if code != http.StatusForbidden {
		t.Fatalf("merge from frozen source: status=%d, want 403", code)
	}

	// Balances moved exactly once.
	_, body = getJSON(t, srv.URL+"/accounts/new-corp/balance")
	if body["balance_cents"] != float64(10000) {
		t.Errorf("new-corp balance = %v, want 10000", body["balance_cents"])
	}
	_, body = getJSON(t, srv.URL+"/accounts/old-corp/balance")
	if body["balance_cents"] != float64(0) {
		t.Errorf("old-corp balance = %v, want 0", body["balance_cents"])
	}

	// Reconcile reports the merge and the frozen source.
	code, body = postJSON(t, srv.URL+"/reconcile", `{}`)
	if code != http.StatusOK {
		t.Fatalf("reconcile: status=%d, want 200", code)
	}
	merges, ok := body["merges"].([]any)
	if !ok || len(merges) != 1 {
		t.Fatalf("reconcile merges = %v, want one", body["merges"])
	}
	m := merges[0].(map[string]any)
	if m["merge_id"] != "mg-1" || m["from_account"] != "old-corp" || m["to_account"] != "new-corp" {
		t.Errorf("reconcile merge = %v", m)
	}

	// Metrics: merges counted, one idempotency hit, one frozen rejection.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	if _, err := io.Copy(&sb, resp.Body); err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	text := sb.String()
	for _, want := range []string{
		"ledger_merges_total 3",
		"ledger_merge_idempotency_hits_total 1",
		"ledger_frozen_rejections_total 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics missing %q\n%s", want, text)
		}
	}
}

func TestMergeOverdraftHTTP(t *testing.T) {
	l := ledger.New(ledger.WithOverdraftProtection("new-corp"))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// old-corp overdrawn by 300; new-corp holds 100 and is protected.
	for _, body := range []string{
		`{"debit_account":"funding","credit_account":"old-corp","amount_cents":300}`,
		`{"debit_account":"new-corp","credit_account":"funding","amount_cents":100}`,
	} {
		if code, _ := postJSON(t, srv.URL+"/entries", body); code != http.StatusCreated {
			t.Fatalf("funding: status=%d, want 201", code)
		}
	}
	code, _ := postJSON(t, srv.URL+"/merges",
		`{"merge_id":"mg-od","from_account":"old-corp","to_account":"new-corp"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("overdrawn merge: status=%d, want 422", code)
	}
}

func TestMergeValidationHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"same account", `{"from_account":"a","to_account":"a"}`, http.StatusBadRequest},
		{"missing source", `{"to_account":"b"}`, http.StatusBadRequest},
		{"missing target", `{"from_account":"a"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := postJSON(t, srv.URL+"/merges", tc.body); code != tc.want {
				t.Fatalf("status=%d, want %d", code, tc.want)
			}
		})
	}
}
