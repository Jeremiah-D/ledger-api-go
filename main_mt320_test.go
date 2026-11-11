package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// mt320HTTPSetup funds a depositor ("saver", registered BIC
// DEUTDEFF) and opens one 90-day, 500 bps term deposit (opened
// 2026-11-01), ready for GET /exports/mt320. The deposit ID ends
// in 4 digits (the :22C: derivation needs them).
func mt320HTTPSetup(t *testing.T) (*ledger.Ledger, ledger.TermDeposit) {
	t.Helper()
	l := ledger.New(ledger.WithTermDepositInterestExpense("td-interest-expense"))
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-saver", DebitAccount: "saver", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN saver: %v", err)
	}
	td, _, err := l.OpenTermDeposit(ledger.TermDeposit{
		ID: "td-mt320-1001", Account: "saver", AmountCents: 400_000,
		Currency: "USD", TermDays: 90, RateBps: 500,
		OpenedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	return l, td
}

// Full MT320 confirmation flow through HTTP: open the deposit,
// export the confirmation, and verify the FIN body, the
// byte-identical repeat, and the metrics counter.
func TestHTTPMT320Export(t *testing.T) {
	l, td := mt320HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	getText := func(t *testing.T, raw string) (int, http.Header, string) {
		t.Helper()
		resp, err := http.Get(raw)
		if err != nil {
			t.Fatalf("GET %s: %v", raw, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", raw, err)
		}
		return resp.StatusCode, resp.Header, string(body)
	}
	q := url.Values{}
	q.Set("deposit_id", td.ID)
	q.Set("counterparty_bic", "NWBKGB2L")

	code, hdr, body := getText(t, srv.URL+"/exports/mt320?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q, want text/plain; charset=utf-8", ct)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I320NWBKGB2LXXXU3003}",
		"{3:{108:td-mt320-1001}}",
		":15A:",
		":20:td-mt320-1001",
		":22A:NEWT",
		":22B:CONF",
		// :22C: = DEUT + NW + 1001 + NWBK + DE.
		":22C:DEUTNW1001NWBKDE",
		":82A:DEUTDEFF",
		":87A:NWBKGB2L",
		":15B:",
		":17R:L",
		":30T:20261101",
		":30V:20261101",
		":30P:20270130",
		":32B:USD4000,00",
		":34E:USD48,60",
		":37G:0,05",
		":14D:ACT/365",
		":15C:",
		":57A:NWBKGB2L",
		":15D:",
		":57A:DEUTDEFF",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	for _, banned := range []string{
		":21:", ":94A:", ":21N:", ":83a:", ":77D:", ":32H:", ":30X:", ":30F:", ":38J:",
		":15F:", ":15G:", ":15H:", ":15I:",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("export must not carry %s, body:\n%s", banned, body)
		}
	}

	// Repeat export is byte-identical: every field is pinned to
	// the stored deposit and the caller-given options, no clock
	// value leaks into the document.
	_, _, body2 := getText(t, srv.URL+"/exports/mt320?"+q.Encode())
	if body2 != body {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export (1 export + 1 repeat
	// = 2).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_mt320_exports_total 2") {
		t.Errorf("metrics missing ledger_mt320_exports_total 2")
	}
}

// Error mapping for GET /exports/mt320: a missing deposit_id or
// counterparty_bic is a 400, an unknown deposit a 404, an
// early-withdrawn deposit a 422, an invalid counterparty BIC a 400
// — and rejected requests still count in the metrics.
func TestHTTPMT320ExportErrors(t *testing.T) {
	l, td := mt320HTTPSetup(t)
	if _, err := l.WithdrawTermDeposit(td.ID, 0, time.Now().UTC()); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	getCode := func(t *testing.T, raw string) int {
		t.Helper()
		resp, err := http.Get(raw)
		if err != nil {
			t.Fatalf("GET %s: %v", raw, err)
		}
		defer resp.Body.Close()
		io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	// Missing deposit_id.
	if code := getCode(t, srv.URL+"/exports/mt320?counterparty_bic=NWBKGB2L"); code != http.StatusBadRequest {
		t.Errorf("missing deposit_id: status=%d, want 400", code)
	}
	// Missing counterparty_bic.
	if code := getCode(t, srv.URL+"/exports/mt320?deposit_id="+td.ID); code != http.StatusBadRequest {
		t.Errorf("missing counterparty_bic: status=%d, want 400", code)
	}
	// Unknown deposit.
	q := url.Values{}
	q.Set("deposit_id", "td-nope")
	q.Set("counterparty_bic", "NWBKGB2L")
	if code := getCode(t, srv.URL+"/exports/mt320?"+q.Encode()); code != http.StatusNotFound {
		t.Errorf("unknown deposit: status=%d, want 404", code)
	}
	// Early-withdrawn deposit.
	q.Set("deposit_id", td.ID)
	if code := getCode(t, srv.URL+"/exports/mt320?"+q.Encode()); code != http.StatusUnprocessableEntity {
		t.Errorf("withdrawn deposit: status=%d, want 422", code)
	}
	// Invalid counterparty BIC.
	q.Set("counterparty_bic", "NWBK")
	if code := getCode(t, srv.URL+"/exports/mt320?"+q.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad counterparty BIC: status=%d, want 400", code)
	}

	// Rejected requests still count: 5 errors above.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(mbody), "ledger_mt320_exports_total 5") {
		t.Errorf("metrics missing ledger_mt320_exports_total 5")
	}
}
