package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// TestMetricsDelegationsTotal scrapes /metrics after granting review
// vote delegations through the ledger API: the ledger is the source of
// truth for ledger_review_delegations_total (LG-138), synced on every
// scrape.
func TestMetricsDelegationsTotal(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, body, _ := getText(t, srv.URL+"/metrics")
	if code != 200 {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	series := parseExposition(t, body)
	if got := series["ledger_review_delegations_total"]; got != 0 {
		t.Fatalf("delegations total before grants = %d, want 0", got)
	}

	ctx := ledger.ContextWithActor(context.Background(), "alice")
	if _, err := l.DelegateVoteCtx(ctx, ledger.DelegationScopeQuorum, "", "dave", time.Time{}); err != nil {
		t.Fatalf("DelegateVote: %v", err)
	}
	if _, err := l.DelegateVoteCtx(ctx, ledger.DelegationScopeChain, "", "erin", time.Time{}); err != nil {
		t.Fatalf("DelegateVote: %v", err)
	}

	code, body, _ = getText(t, srv.URL+"/metrics")
	if code != 200 {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	series = parseExposition(t, body)
	if got := series["ledger_review_delegations_total"]; got != 2 {
		t.Errorf("ledger_review_delegations_total = %d, want 2", got)
	}
	if !strings.Contains(body, "ledger_review_delegations_total") {
		t.Error("exposition missing ledger_review_delegations_total series")
	}
}
