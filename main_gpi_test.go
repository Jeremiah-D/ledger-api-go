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

// gpiTestServer builds a server with a funded payer, so payouts can
// be drafted through HTTP.
func gpiTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New(ledger.WithPayoutClearingAccount("payout-clearing"))
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-saver", DebitAccount: "saver", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

// draftPayoutHTTP drafts a payout and returns its UETR.
func draftPayoutHTTP(t *testing.T, srvURL, id string) string {
	t.Helper()
	code, body := postJSON(t, srvURL+"/payouts",
		`{"id":"`+id+`","payee_account":"saver","amount_cents":250000,"destination":"FPS:6222000012345678","maker":"alice"}`)
	if code != http.StatusCreated {
		t.Fatalf("draft payout: status=%d body=%v, want 201", code, body)
	}
	uetr, ok := body["uetr"].(string)
	if !ok || uetr == "" {
		t.Fatalf("payout response has no uetr: %v", body)
	}
	return uetr
}

func getJSONMap(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, body
}

// Full gpi flow through HTTP: draft opens a tracked record, the
// operator advances it, and the query endpoint discloses it.
func TestHTTPGPITrackLifecycle(t *testing.T) {
	srv := gpiTestServer(t)
	uetr := draftPayoutHTTP(t, srv.URL, "po-gpi-http-1")

	code, body := getJSONMap(t, srv.URL+"/gpi/track/"+uetr)
	if code != http.StatusOK {
		t.Fatalf("track query: status=%d body=%v, want 200", code, body)
	}
	if body["status"] != "tracked" {
		t.Errorf("status = %v, want tracked", body["status"])
	}
	if body["payout_id"] != "po-gpi-http-1" {
		t.Errorf("payout_id = %v, want po-gpi-http-1", body["payout_id"])
	}

	// Unknown UETR: 404.
	if code, _ := getJSONMap(t, srv.URL+"/gpi/track/no-such-uetr"); code != http.StatusNotFound {
		t.Errorf("unknown UETR: status=%d, want 404", code)
	}

	// Operator advance to ACSP.
	code, body = postJSONWithActor(t, srv.URL+"/gpi/track/"+uetr+"/advance",
		`{"status":"ACSP"}`, "ops")
	if code != http.StatusOK || body["status"] != "ACSP" {
		t.Fatalf("advance: status=%d body=%v, want 200 ACSP", code, body)
	}

	// Unknown status: 422.
	if code, _ := postJSONWithActor(t, srv.URL+"/gpi/track/"+uetr+"/advance",
		`{"status":"BOGUS"}`, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("bad status: status=%d, want 422", code)
	}

	// RJCT without a reason: 422 (fail-closed).
	if code, _ := postJSONWithActor(t, srv.URL+"/gpi/track/"+uetr+"/advance",
		`{"status":"RJCT"}`, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("RJCT without reason: status=%d, want 422", code)
	}

	// RJCT with the bank's reason code.
	code, body = postJSONWithActor(t, srv.URL+"/gpi/track/"+uetr+"/advance",
		`{"status":"RJCT","reason_code":"AC04"}`, "ops")
	if code != http.StatusOK || body["reason_code"] != "AC04" {
		t.Fatalf("reject: status=%d body=%v, want 200 with reason", code, body)
	}

	// Terminal: no move out of RJCT.
	if code, _ := postJSONWithActor(t, srv.URL+"/gpi/track/"+uetr+"/advance",
		`{"status":"ACSC"}`, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("RJCT -> ACSC: status=%d, want 422", code)
	}

	// The query discloses the full history.
	_, body = getJSONMap(t, srv.URL+"/gpi/track/"+uetr)
	hist, ok := body["history"].([]any)
	if !ok || len(hist) != 2 {
		t.Errorf("history = %v, want 2 entries", body["history"])
	}
}

// POST /gpi/link joins a transfer to a UETR: unknown UETRs 404, a
// transfer already linked elsewhere 409s.
func TestHTTPGPILink(t *testing.T) {
	srv := gpiTestServer(t)
	uetr1 := draftPayoutHTTP(t, srv.URL, "po-gpi-http-2")
	uetr2 := draftPayoutHTTP(t, srv.URL, "po-gpi-http-3")

	code, body := postJSONWithActor(t, srv.URL+"/gpi/link",
		`{"uetr":"`+uetr1+`","transfer_id":"tx-1"}`, "ops")
	if code != http.StatusOK || body["transfer_id"] != "tx-1" {
		t.Fatalf("link: status=%d body=%v, want 200", code, body)
	}

	// Same transfer under a different UETR: conflict.
	if code, _ := postJSONWithActor(t, srv.URL+"/gpi/link",
		`{"uetr":"`+uetr2+`","transfer_id":"tx-1"}`, "ops"); code != http.StatusConflict {
		t.Errorf("conflicting link: status=%d, want 409", code)
	}

	// Unknown UETR: 404.
	if code, _ := postJSONWithActor(t, srv.URL+"/gpi/link",
		`{"uetr":"no-such","transfer_id":"tx-9"}`, "ops"); code != http.StatusNotFound {
		t.Errorf("unknown UETR link: status=%d, want 404", code)
	}

	// Empty fields: 400.
	if code, _ := postJSONWithActor(t, srv.URL+"/gpi/link",
		`{"uetr":"`+uetr1+`"}`, "ops"); code != http.StatusBadRequest {
		t.Errorf("empty transfer_id: status=%d, want 400", code)
	}
}

// The gpi counters surface on /metrics: tracked moves when payouts
// are drafted, settled/rejected when tracks advance.
func TestHTTPGPIMetrics(t *testing.T) {
	srv := gpiTestServer(t)
	uetr := draftPayoutHTTP(t, srv.URL, "po-gpi-http-4")
	if code, _ := postJSONWithActor(t, srv.URL+"/gpi/track/"+uetr+"/advance",
		`{"status":"ACCP"}`, "ops"); code != http.StatusOK {
		t.Fatalf("advance to ACCP: status=%d", code)
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	out := sb.String()
	for _, want := range []string{
		"ledger_gpi_tracked_total 1",
		"ledger_gpi_settled_total 1",
		"ledger_gpi_rejected_total 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}
