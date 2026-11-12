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

// mt321HTTPSetup funds a depositor ("saver", registered BIC
// DEUTDEFF, IBAN DE89370400440532013000) and opens one 90-day,
// 500 bps term deposit (opened 2026-11-01), ready for GET
// /exports/mt321. The deposit ID ends in 4 digits (the
// :20C::LDCR// derivation needs them).
func mt321HTTPSetup(t *testing.T) (*ledger.Ledger, ledger.TermDeposit) {
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
		ID: "td-mt321-1001", Account: "saver", AmountCents: 400_000,
		Currency: "USD", TermDays: 90, RateBps: 500,
		OpenedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	return l, td
}

// Full MT321 settlement-instruction flow through HTTP: open the
// deposit, export the instruction, and verify the ISO 15022 body,
// the byte-identical repeat, and the metrics counter.
func TestHTTPMT321Export(t *testing.T) {
	l, td := mt321HTTPSetup(t)
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
	q.Set("custodian_bic", "CITIUS33")
	q.Set("counterparty_bic", "NWBKGB2L")

	code, hdr, body := getText(t, srv.URL+"/exports/mt321?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q, want text/plain; charset=utf-8", ct)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I321CITIUS33XXXU3003}",
		"{3:{108:td-mt321-1001}}",
		":16R:GENL",
		":20C::SEME//td-mt321-1001",
		// :20C::LDCR// re-derives the MT320 :22C: = DEUT + NW
		// + 1001 + NWBK + DE.
		":20C::LDCR//DEUTNW1001NWBKDE",
		":23G:NEWM",
		":22H::TLDE//CONF",
		":16S:GENL",
		":16R:TRADDET",
		":20C::CONU//td-mt321-1001",
		":22H::DPLO//LEND",
		":22H::MICO//A009",
		":98A::TRAD//20261101",
		":98A::VALU//20261101",
		":98A::MATU//20270130",
		":19A::PRIN//USD4000,00",
		":19A::SETT//USD4000,00",
		":92A::INTR//0,05",
		":95P::LDCO//NWBKGB2L",
		":95P::INVE//DEUTDEFF",
		":97A::SAFE//DE89370400440532013000",
		":16S:TRADDET",
		":16R:SETT",
		":22H::PRIT//APFM",
		":95P::ACCW//NWBKGB2L",
		":95P::CDEA//DEUTDEFF",
		":16S:SETT",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	for _, banned := range []string{
		":15A:", ":99B:", ":13A:", ":20C::POOL", ":19A::CINT:",
		":19A::NINT:", ":19A::RODI:", ":97A::CASH:", ":70C:",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("export must not carry %s, body:\n%s", banned, body)
		}
	}

	// Repeat export is byte-identical: every field is pinned to
	// the stored deposit and the caller-given options, no clock
	// value leaks into the document.
	_, _, body2 := getText(t, srv.URL+"/exports/mt321?"+q.Encode())
	if body2 != body {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export (1 export + 1 repeat
	// = 2).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_mt321_exports_total 2") {
		t.Errorf("metrics missing ledger_mt321_exports_total 2")
	}
}

// Error mapping for GET /exports/mt321: a missing deposit_id,
// custodian_bic, or counterparty_bic is a 400, an unknown deposit
// a 404, an early-withdrawn deposit a 422, an invalid custodian or
// counterparty BIC a 400 — and rejected requests still count in
// the metrics.
func TestHTTPMT321ExportErrors(t *testing.T) {
	l, td := mt321HTTPSetup(t)
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
	if code := getCode(t, srv.URL+"/exports/mt321?custodian_bic=CITIUS33&counterparty_bic=NWBKGB2L"); code != http.StatusBadRequest {
		t.Errorf("missing deposit_id: status=%d, want 400", code)
	}
	// Missing custodian_bic.
	if code := getCode(t, srv.URL+"/exports/mt321?deposit_id="+td.ID+"&counterparty_bic=NWBKGB2L"); code != http.StatusBadRequest {
		t.Errorf("missing custodian_bic: status=%d, want 400", code)
	}
	// Missing counterparty_bic.
	if code := getCode(t, srv.URL+"/exports/mt321?deposit_id="+td.ID+"&custodian_bic=CITIUS33"); code != http.StatusBadRequest {
		t.Errorf("missing counterparty_bic: status=%d, want 400", code)
	}
	// Unknown deposit.
	q := url.Values{}
	q.Set("deposit_id", "td-nope")
	q.Set("custodian_bic", "CITIUS33")
	q.Set("counterparty_bic", "NWBKGB2L")
	if code := getCode(t, srv.URL+"/exports/mt321?"+q.Encode()); code != http.StatusNotFound {
		t.Errorf("unknown deposit: status=%d, want 404", code)
	}
	// Early-withdrawn deposit.
	q.Set("deposit_id", td.ID)
	if code := getCode(t, srv.URL+"/exports/mt321?"+q.Encode()); code != http.StatusUnprocessableEntity {
		t.Errorf("withdrawn deposit: status=%d, want 422", code)
	}
	// Invalid custodian BIC.
	q.Set("custodian_bic", "CITI")
	if code := getCode(t, srv.URL+"/exports/mt321?"+q.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad custodian BIC: status=%d, want 400", code)
	}
	// Invalid counterparty BIC.
	q.Set("custodian_bic", "CITIUS33")
	q.Set("counterparty_bic", "NWBK")
	if code := getCode(t, srv.URL+"/exports/mt321?"+q.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad counterparty BIC: status=%d, want 400", code)
	}

	// Rejected requests still count: 7 errors above.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(mbody), "ledger_mt321_exports_total 7") {
		t.Errorf("metrics missing ledger_mt321_exports_total 7")
	}
}
