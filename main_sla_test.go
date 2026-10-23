package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// slaTestSetup builds a server with a funded payer on the default
// tenant ledger and hands back both, so tests can drive the HTTP API
// and configure the ledger directly (e.g. a millisecond SLA).
func slaTestSetup(t *testing.T) (*httptest.Server, *ledger.Ledger) {
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
	return srv, l
}

func submitPayoutWithRailHTTP(t *testing.T, srvURL, id, rail string) {
	t.Helper()
	body := `{"id":"` + id + `","payee_account":"saver","amount_cents":250000,` +
		`"destination":"FPS:6222000012345678","maker":"alice"`
	if rail != "" {
		body += `,"rail":"` + rail + `"`
	}
	body += `}`
	code, _ := postJSON(t, srvURL+"/payouts", body)
	if code != http.StatusCreated {
		t.Fatalf("draft payout %s: status=%d, want 201", id, code)
	}
	code, _ = postJSON(t, srvURL+"/payouts/"+id+"/approve", `{"approver":"bob"}`)
	if code != http.StatusOK {
		t.Fatalf("approve payout %s: status=%d, want 200", id, code)
	}
	code, _ = postJSON(t, srvURL+"/payouts/"+id+"/submit", `{}`)
	if code != http.StatusOK {
		t.Fatalf("submit payout %s: status=%d, want 200", id, code)
	}
}

// POST /sla/check through HTTP: with a millisecond fednow SLA the
// submitted payout breaches within one sweep of the check.
func TestSLACheckEndpoint_Breach(t *testing.T) {
	srv, l := slaTestSetup(t)
	if err := l.SetSLAConfig("fednow", time.Millisecond); err != nil {
		t.Fatalf("SetSLAConfig: %v", err)
	}
	submitPayoutWithRailHTTP(t, srv.URL, "po-sla-http", "fednow")
	time.Sleep(50 * time.Millisecond)
	code, body := postJSON(t, srv.URL+"/sla/check", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /sla/check: status=%d, want 200", code)
	}
	breached, ok := body["breached"].([]any)
	if !ok || len(breached) != 1 {
		t.Fatalf("breached = %v, want one breach", body["breached"])
	}
	entry, ok := breached[0].(map[string]any)
	if !ok || entry["id"] != "po-sla-http" || entry["rail"] != "fednow" || entry["kind"] != "payout" {
		t.Errorf("breach entry = %v, want po-sla-http/payout/fednow", breached[0])
	}
	// A second check fires nothing: each watch breaches at most once.
	code, body = postJSON(t, srv.URL+"/sla/check", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /sla/check (2nd): status=%d, want 200", code)
	}
	if breached, _ = body["breached"].([]any); len(breached) != 0 {
		t.Errorf("second check breached = %v, want none", body["breached"])
	}
	// The breach is on /metrics as ledger_sla_breaches_total{rail}.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, `ledger_sla_breaches_total{rail="fednow"} 1`) {
		t.Errorf("/metrics missing the fednow breach series:\n%s", excerpt(text, "ledger_sla_breaches_total"))
	}
}

// A payment inside its SLA breaches nothing: the check returns an
// empty list.
func TestSLACheckEndpoint_NoBreach(t *testing.T) {
	srv, _ := slaTestSetup(t)
	submitPayoutWithRailHTTP(t, srv.URL, "po-sla-ok", "sepa")
	code, body := postJSON(t, srv.URL+"/sla/check", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /sla/check: status=%d, want 200", code)
	}
	if breached, _ := body["breached"].([]any); len(breached) != 0 {
		t.Errorf("breached = %v, want none", body["breached"])
	}
}

// excerpt returns the lines of text containing marker, for failure
// output.
func excerpt(text, marker string) string {
	var sb strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, marker) {
			sb.WriteString(line + "\n")
		}
	}
	return sb.String()
}
