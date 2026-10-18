package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// reviewQuorumHTTPServer returns a test server with a 10000-cent review
// threshold, alice funded with 100000 cents, and a 2-of-3 quorum over
// alice/bob/carol armed on the ledger.
func reviewQuorumHTTPServer(t *testing.T, rejectDecisive bool) *httptest.Server {
	t.Helper()
	l := ledger.New(ledger.WithReviewThreshold(10000))
	if err := l.SetReviewQuorumPolicy([]string{"alice", "bob", "carol"}, 2, rejectDecisive); err != nil {
		t.Fatalf("SetReviewQuorumPolicy: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":100000}`)
	if code != http.StatusCreated {
		srv.Close()
		t.Fatalf("funding post: status=%d, want 201", code)
	}
	return srv
}

func TestReviewQuorumHTTPVoteFlow(t *testing.T) {
	srv := reviewQuorumHTTPServer(t, true)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-qh1","from_account":"alice","to_account":"bob","amount_cents":30000}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201", code)
	}

	// First vote: 200, still pending, live tally on the receipt.
	code, body := postJSON(t, srv.URL+"/transfers/tx-qh1/approve", `{"reviewer":"alice"}`)
	if code != http.StatusOK {
		t.Fatalf("vote 1: status=%d, want 200 (body=%v)", code, body)
	}
	if body["review_status"] != "pending_review" {
		t.Errorf("vote 1 review_status = %v, want pending_review", body["review_status"])
	}
	tally, ok := body["quorum_tally"].(map[string]any)
	if !ok {
		t.Fatalf("vote 1 has no quorum_tally (body=%v)", body)
	}
	if tally["outcome"] != "recorded" || tally["last_voter"] != "alice" {
		t.Errorf("vote 1 tally = %v, want outcome recorded by alice", tally)
	}
	if tally["threshold_n"] != float64(2) {
		t.Errorf("vote 1 threshold_n = %v, want 2", tally["threshold_n"])
	}

	// A non-member vote is forbidden.
	code, _ = postJSON(t, srv.URL+"/transfers/tx-qh1/approve", `{"reviewer":"mallory"}`)
	if code != http.StatusForbidden {
		t.Errorf("non-member vote: status=%d, want 403", code)
	}

	// Second vote reaches the threshold: the transfer settles.
	code, body = postJSON(t, srv.URL+"/transfers/tx-qh1/approve", `{"reviewer":"bob"}`)
	if code != http.StatusOK {
		t.Fatalf("vote 2: status=%d, want 200 (body=%v)", code, body)
	}
	if body["review_status"] != "approved" {
		t.Errorf("vote 2 review_status = %v, want approved", body["review_status"])
	}
	tally, ok = body["quorum_tally"].(map[string]any)
	if !ok || tally["outcome"] != "settled" {
		t.Errorf("vote 2 tally = %v, want outcome settled", body["quorum_tally"])
	}

	// Money moved.
	_, body = getJSON(t, srv.URL+"/accounts/alice/balance")
	if body["balance_cents"] != float64(70000) {
		t.Errorf("alice balance = %v, want 70000", body["balance_cents"])
	}

	// The vote metric carries every vote with its outcome; the settle
	// replay of an approved review counts nothing new.
	code, body = postJSON(t, srv.URL+"/transfers/tx-qh1/approve", `{"reviewer":"carol"}`)
	if code != http.StatusOK || body["duplicate"] != true {
		t.Errorf("re-approve: status=%d duplicate=%v, want 200/true", code, body["duplicate"])
	}
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	for _, want := range []string{
		`ledger_review_quorum_votes_total{approver="alice",vote="approve",outcome="recorded"} 1`,
		`ledger_review_quorum_votes_total{approver="bob",vote="approve",outcome="settled"} 1`,
	} {
		if !strings.Contains(mbody, want) {
			t.Errorf("/metrics missing %q:\n%s", want, mbody)
		}
	}
	m := parseExposition(t, mbody)
	if m["ledger_review_approved_total"] != 1 {
		t.Errorf("ledger_review_approved_total = %d, want 1", m["ledger_review_approved_total"])
	}
}

func TestReviewQuorumHTTPRejectDecisive(t *testing.T) {
	srv := reviewQuorumHTTPServer(t, true)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-qh2","from_account":"alice","to_account":"bob","amount_cents":20000}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201", code)
	}
	// A non-member reject is forbidden.
	code, _ = postJSON(t, srv.URL+"/transfers/tx-qh2/reject", `{"reviewer":"mallory"}`)
	if code != http.StatusForbidden {
		t.Errorf("non-member reject: status=%d, want 403", code)
	}
	// The member's reject vote is decisive.
	code, body := postJSON(t, srv.URL+"/transfers/tx-qh2/reject",
		`{"reviewer":"carol","reason":"sanctions screen"}`)
	if code != http.StatusOK {
		t.Fatalf("reject vote: status=%d, want 200 (body=%v)", code, body)
	}
	if body["status"] != "rejected" {
		t.Errorf("status = %v, want rejected", body["status"])
	}
	if votes, ok := body["reject_votes"].([]any); !ok || len(votes) != 1 || votes[0] != "carol" {
		t.Errorf("reject_votes = %v, want [carol]", body["reject_votes"])
	}

	code, mbody, _ := getText(t, srv.URL+"/metrics")
	m := parseExposition(t, mbody)
	if m["ledger_review_rejected_total"] != 1 {
		t.Errorf("ledger_review_rejected_total = %d, want 1", m["ledger_review_rejected_total"])
	}
	if want := `ledger_review_quorum_votes_total{approver="carol",vote="reject",outcome="decisive"} 1`; !strings.Contains(mbody, want) {
		t.Errorf("/metrics missing %q", want)
	}
}

func TestReviewQuorumHTTPRejectNonDecisive(t *testing.T) {
	srv := reviewQuorumHTTPServer(t, false)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-qh3","from_account":"alice","to_account":"bob","amount_cents":20000}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201", code)
	}
	// The reject vote is recorded as dissent; the review stays pending
	// and the rejection total is untouched.
	code, body := postJSON(t, srv.URL+"/transfers/tx-qh3/reject",
		`{"reviewer":"carol","reason":"looks odd"}`)
	if code != http.StatusOK {
		t.Fatalf("reject vote: status=%d, want 200 (body=%v)", code, body)
	}
	if body["status"] != "pending_review" {
		t.Errorf("status = %v, want pending_review", body["status"])
	}
	// The review can still reach the approval quorum.
	code, _ = postJSON(t, srv.URL+"/transfers/tx-qh3/approve", `{"reviewer":"alice"}`)
	if code != http.StatusOK {
		t.Fatalf("approve vote: status=%d, want 200", code)
	}
	code, body = postJSON(t, srv.URL+"/transfers/tx-qh3/approve", `{"reviewer":"bob"}`)
	if code != http.StatusOK || body["review_status"] != "approved" {
		t.Fatalf("settling vote: status=%d review_status=%v, want 200/approved", code, body["review_status"])
	}

	code, mbody, _ := getText(t, srv.URL+"/metrics")
	m := parseExposition(t, mbody)
	if m["ledger_review_rejected_total"] != 0 {
		t.Errorf("ledger_review_rejected_total = %d, want 0 (vote was dissent, not a rejection)", m["ledger_review_rejected_total"])
	}
	if want := `ledger_review_quorum_votes_total{approver="carol",vote="reject",outcome="recorded"} 1`; !strings.Contains(mbody, want) {
		t.Errorf("/metrics missing %q", want)
	}
}
