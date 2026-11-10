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

// mt900910TestServer builds an HTTP test server whose ledger has a
// funded payee account ("saver", with the sender bank's BIC
// registered) and a second funded payee ("saver-nobic", with no IBAN
// registered), plus the payout clearing account. It returns the
// server and the ledger so tests can settle payouts directly: there
// is no HTTP settle endpoint, and the MT900/MT910 gate is "settled".
func mt900910TestServer(t *testing.T) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	l := ledger.New(ledger.WithPayoutClearingAccount("payout-clearing"))
	for _, acct := range []string{"saver", "saver-nobic"} {
		if _, _, err := l.Post(ledger.JournalEntry{
			ID: "fund-" + acct, DebitAccount: ledger.AccountID(acct),
			CreditAccount: "equity", AmountCents: 1_000_000,
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed Post %s: %v", acct, err)
		}
	}
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "BANKUS33"); err != nil {
		t.Fatalf("SetAccountIBAN: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv, l
}

// settledPayoutHTTP settles a payout through the ledger API (draft,
// approve, submit, then settle against a matching statement line)
// and returns its ID. The HTTP layer has no settle endpoint.
func settledPayout(t *testing.T, l *ledger.Ledger, id, payee string) {
	t.Helper()
	if _, dup, err := l.CreatePayout(ledger.PayoutInstruction{
		ID: id, PayeeAccount: ledger.AccountID(payee),
		AmountCents: 250000, Destination: "FPS:6222000012345678",
		Maker: "alice",
	}); err != nil {
		t.Fatalf("CreatePayout: %v", err)
	} else if dup {
		t.Fatalf("duplicate = true on first create of %s", id)
	}
	if _, err := l.ApprovePayout(id, "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	p, err := l.SubmitPayout(id)
	if err != nil {
		t.Fatalf("SubmitPayout: %v", err)
	}
	line := ledger.StatementLine{
		ID: "stmt-" + id, Account: ledger.AccountID(payee),
		Currency: p.Currency, Direction: ledger.StatementDirectionOut,
		AmountCents: p.AmountCents, PostedAt: p.SubmittedAt,
		Memo: "payout:" + id,
	}
	if _, err := l.SettlePayout(id, line, ledger.StatementMatchOptions{}); err != nil {
		t.Fatalf("SettlePayout: %v", err)
	}
}

func postTextResp(t *testing.T, url, payload string) (int, string, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

// Full MT900/MT910 flow through HTTP: settle a payout, export the
// debit confirmation, and exercise the error mapping.
func TestHTTPMT900910Export(t *testing.T) {
	srv, l := mt900910TestServer(t)
	settledPayout(t, l, "po-mt900910-1", "saver")

	// Malformed JSON: 400.
	code, _, _ := postTextResp(t, srv.URL+"/exports/mt900910", `{`)
	if code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d, want 400", code)
	}
	// Trailing garbage after the JSON value: 400.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-1","receiver_lt":"BANKDEFFXXXX"} trailing`)
	if code != http.StatusBadRequest {
		t.Errorf("trailing garbage: status=%d, want 400", code)
	}
	// Unknown field: 400 (strict decode).
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-1","receiver_lt":"BANKDEFFXXXX","swift_code":"BANKDEFF"}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}
	// Unknown payout: 404.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"no-such-payout","receiver_lt":"BANKDEFFXXXX"}`)
	if code != http.StatusNotFound {
		t.Errorf("unknown payout: status=%d, want 404", code)
	}
	// Missing receiver_lt: 400.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-1"}`)
	if code != http.StatusBadRequest {
		t.Errorf("missing receiver_lt: status=%d, want 400", code)
	}
	// Missing payout_id: 400.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt900910",
		`{"receiver_lt":"BANKDEFFXXXX"}`)
	if code != http.StatusBadRequest {
		t.Errorf("missing payout_id: status=%d, want 400", code)
	}
	// Invalid message_type: 400 (only 900/910 exist).
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-1","receiver_lt":"BANKDEFFXXXX","message_type":"999"}`)
	if code != http.StatusBadRequest {
		t.Errorf("invalid message_type: status=%d, want 400", code)
	}
	// Invalid value_date: 400.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-1","receiver_lt":"BANKDEFFXXXX","value_date":"22/10/2026"}`)
	if code != http.StatusBadRequest {
		t.Errorf("invalid value_date: status=%d, want 400", code)
	}

	// Happy path: MT900 debit confirmation (the default message
	// type), Content-Type text/plain.
	code, ctype, msg := postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-1","receiver_lt":"BANKDEFF"}`)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, msg)
	}
	if ctype != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ctype)
	}
	for _, want := range []string{
		"{1:F01BANKUS33XXX0000000000}",
		"{2:I900BANKDEFFXXXU3003}",
		"{3:{108:po-mt900910-1}}",
		":20:po-mt900910-1",
		":21:NONREF",
		":25:saver",
		":32A:" + time.Now().Format("060102") + "USD2500,00",
		"-}",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q", want)
		}
	}
	if strings.Contains(msg, ":52A:") || strings.Contains(msg, ":72:") {
		t.Errorf("message carries unexpected optional fields:\n%s", msg)
	}

	// Repeat export: byte-identical, and the payout is untouched.
	code, _, again := postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-1","receiver_lt":"BANKDEFF"}`)
	if code != http.StatusOK {
		t.Fatalf("repeat export: status=%d, want 200", code)
	}
	if again != msg {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", msg, again)
	}
	if code, payout := getJSONMap(t, srv.URL+"/payouts/po-mt900910-1"); code != http.StatusOK || payout["status"] != "settled" {
		t.Errorf("payout after export: status=%d body=%v, want 200 settled", code, payout["status"])
	}

	// MT910 credit confirmation: same payout, other direction.
	code, _, msg910 := postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-1","receiver_lt":"BANKDEFF","message_type":"910"}`)
	if code != http.StatusOK {
		t.Fatalf("export 910: status=%d body=%s, want 200", code, msg910)
	}
	if !strings.Contains(msg910, "{2:I910BANKDEFFXXXU3003}") {
		t.Errorf("910 message is missing I910 header:\n%s", msg910)
	}
	if !strings.Contains(msg910, ":20:po-mt900910-1") {
		t.Errorf("910 message is missing :20: payout ID:\n%s", msg910)
	}

	// Draft payout: 422, money has not moved yet.
	draftPayoutHTTP(t, srv.URL, "po-mt900910-2")
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-2","receiver_lt":"BANKDEFFXXXX"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("draft: status=%d, want 422", code)
	}

	// Submitted payout: 422.
	submittedPayoutHTTP(t, srv.URL, "po-mt900910-3")
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-3","receiver_lt":"BANKDEFFXXXX"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("submitted: status=%d, want 422", code)
	}

	// Failed payout: 422.
	submittedPayoutHTTP(t, srv.URL, "po-mt900910-4")
	if _, err := l.FailPayout("po-mt900910-4", "bank rejected"); err != nil {
		t.Fatalf("FailPayout: %v", err)
	}
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-4","receiver_lt":"BANKDEFFXXXX"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("failed: status=%d, want 422", code)
	}

	// Cancelled payout: 422.
	draftPayoutHTTP(t, srv.URL, "po-mt900910-5")
	if _, err := l.CancelPayout("po-mt900910-5"); err != nil {
		t.Fatalf("CancelPayout: %v", err)
	}
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-5","receiver_lt":"BANKDEFFXXXX"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("cancelled: status=%d, want 422", code)
	}
}

// No sender BIC anywhere: the payee account has no registered IBAN
// and no sender_lt was passed, so the export refuses fail-closed
// with 400; passing sender_lt explicitly fixes it.
func TestHTTPMT900910ExportNoSenderBIC(t *testing.T) {
	srv, l := mt900910TestServer(t)
	settledPayout(t, l, "po-mt900910-nb", "saver-nobic")

	code, _, body := postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-nb","receiver_lt":"BANKDEFFXXXX"}`)
	if code != http.StatusBadRequest {
		t.Errorf("no sender BIC: status=%d body=%s, want 400", code, body)
	}
	code, _, msg := postTextResp(t, srv.URL+"/exports/mt900910",
		`{"payout_id":"po-mt900910-nb","receiver_lt":"BANKDEFFXXXX","sender_lt":"BANKGB22"}`)
	if code != http.StatusOK {
		t.Fatalf("explicit sender_lt: status=%d body=%s, want 200", code, msg)
	}
	if !strings.Contains(msg, "{1:F01BANKGB22XXX0000000000}") {
		t.Errorf("message is missing sender LT block 1:\n%s", msg)
	}
}

// Full options through HTTP: related reference, account override,
// ordering institution, :72: lines, and an explicit value date.
func TestHTTPMT900910ExportFullOptions(t *testing.T) {
	srv, l := mt900910TestServer(t)
	settledPayout(t, l, "po-mt900910-opts", "saver")

	code, _, msg := postTextResp(t, srv.URL+"/exports/mt900910", `{
		"payout_id":"po-mt900910-opts",
		"message_type":"910",
		"receiver_lt":"BANKDEFF",
		"sender_lt":"BANKGB22",
		"related_reference":"RELREF12345678",
		"account":"GB29NWBK60161331926819",
		"ordering_institution_bic":"DEUTDEFF",
		"sender_to_receiver_info":["confirm credit","ref 42"],
		"value_date":"2026-10-22"
	}`)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, msg)
	}
	for _, want := range []string{
		"{1:F01BANKGB22XXX0000000000}",
		"{2:I910BANKDEFFXXXU3003}",
		":20:po-mt900910-opts",
		":21:RELREF12345678",
		":25:GB29NWBK60161331926819",
		":32A:261022USD2500,00",
		":52A:DEUTDEFF",
		":72:confirm credit",
		"ref 42",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q:\n%s", want, msg)
		}
	}
}
