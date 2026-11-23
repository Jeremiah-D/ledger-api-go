package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// postCAMT026ExportJSON posts a JSON export request to
// /exports/camt026 with an x-ledger-actor header and returns the
// status, the content type, and the raw body.
func postCAMT026ExportJSON(t *testing.T, url, body, actor string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
}

// Full flow through HTTP: export the ledger's own camt.026 report
// about a submitted payout, replay the same reason, and the
// metrics counter moves.
func TestHTTPCamt026Export(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-camt026-exp-http-1"
	uetr := submittedPayoutHTTP(t, srv.URL, payoutID)
	exportURL := srv.URL + "/exports/camt026"

	body, err := json.Marshal(camt026ExportRequest{
		Reference:     uetr,
		Justification: "missing-or-incorrect",
		ReasonCode:    "MS01",
		ReasonInfo:    "missing creditor account",
		CaseID:        "case-9",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	code, ctype, xmlBody := postCAMT026ExportJSON(t, exportURL, string(body), "bank-ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/camt026: status=%d body=%s", code, xmlBody)
	}
	if !strings.HasPrefix(ctype, "application/xml") {
		t.Errorf("content-type = %q, want application/xml", ctype)
	}
	rep, err := ledger.ParseCAMT026([]byte(xmlBody))
	if err != nil {
		t.Fatalf("ParseCAMT026(own export): %v", err)
	}
	if rep.UETR != uetr || rep.EndToEndID != payoutID {
		t.Errorf("references = %q/%q, want %q/%q", rep.UETR, rep.EndToEndID, uetr, payoutID)
	}
	if rep.UnderlyingKind != ledger.CAMT026UnderlyingInitiation {
		t.Errorf("kind = %q, want initiation", rep.UnderlyingKind)
	}
	if rep.Justification != ledger.CAMT026JustificationMissingOrIncorrect || rep.ReasonCode != "MS01" || rep.ReasonInfo != "missing creditor account" {
		t.Errorf("reason = %q/%q/%q, want missing-or-incorrect/MS01/missing creditor account", rep.Justification, rep.ReasonCode, rep.ReasonInfo)
	}
	if rep.CaseID != "case-9" {
		t.Errorf("case = %q, want case-9", rep.CaseID)
	}
	if !rep.HasAmount || rep.AmountCents != 250_000 || rep.Currency != "USD" {
		t.Errorf("amount = %v/%d/%q, want true/250000/USD", rep.HasAmount, rep.AmountCents, rep.Currency)
	}
	// No IBAN is registered on the gpi test server: the assignor
	// defaults to the payee account, the assignee to the payout
	// destination.
	if rep.Assignor != "saver" || rep.Assignee != "FPS:6222000012345678" {
		t.Errorf("assignor/assignee = %q/%q, want saver/FPS:6222000012345678", rep.Assignor, rep.Assignee)
	}
	msgID := rep.MsgID

	// The payout itself is untouched: still submitted.
	code, payout := getJSON(t, srv.URL+"/payouts/"+payoutID)
	if code != http.StatusOK {
		t.Fatalf("GET /payouts/%s: status=%d", payoutID, code)
	}
	if payout["status"] != "submitted" {
		t.Errorf("payout status = %v, want submitted", payout["status"])
	}

	// Replay the same reason: byte-identical, same MsgId.
	code, _, xmlBody2 := postCAMT026ExportJSON(t, exportURL, string(body), "bank-ops")
	if code != http.StatusOK {
		t.Fatalf("replay POST /exports/camt026: status=%d body=%s", code, xmlBody2)
	}
	if xmlBody2 != xmlBody {
		t.Error("replay export rendered different bytes, want byte-identical")
	}
	rep2, err := ledger.ParseCAMT026([]byte(xmlBody2))
	if err != nil {
		t.Fatalf("ParseCAMT026(replay): %v", err)
	}
	if rep2.MsgID != msgID {
		t.Errorf("replay msg_id = %q, want %q", rep2.MsgID, msgID)
	}

	// Metrics: both exports were counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_camt026_exports_total 2") {
		t.Errorf("metrics exposition missing ledger_camt026_exports_total 2:\n%s", metricsBody)
	}
}

// Error mapping: a malformed body or any invalid field is a 400,
// an unknown reference is a 404. (A MsgId reused for different
// terms is a 422, but the MsgId derives from the terms, so it is
// unreachable through the public API — the ledger-level conflict
// test seeds the registry directly.)
func TestHTTPCamt026ExportErrors(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-camt026-exp-http-2"
	uetr := submittedPayoutHTTP(t, srv.URL, payoutID)
	exportURL := srv.URL + "/exports/camt026"

	if code, _, body := postCAMT026ExportJSON(t, exportURL, `{"reference":`, "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d body=%s, want 400", code, body)
	}
	if code, _, body := postCAMT026ExportJSON(t, exportURL, `{"reference":"x","justification":"missing-or-incorrect","reason_code":"MS01","surprise":true}`, "ops"); code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d body=%s, want 400", code, body)
	}
	if code, _, body := postCAMT026ExportJSON(t, exportURL, `{"justification":"missing-or-incorrect","reason_code":"MS01"}`, "ops"); code != http.StatusBadRequest {
		t.Errorf("empty reference: status=%d body=%s, want 400", code, body)
	}
	if code, _, body := postCAMT026ExportJSON(t, exportURL, `{"reference":"`+uetr+`","reason_code":"MS01"}`, "ops"); code != http.StatusBadRequest {
		t.Errorf("missing justification: status=%d body=%s, want 400", code, body)
	}
	if code, _, body := postCAMT026ExportJSON(t, exportURL, `{"reference":"`+uetr+`","justification":"missing-or-incorrect"}`, "ops"); code != http.StatusBadRequest {
		t.Errorf("missing-or-incorrect without code: status=%d body=%s, want 400", code, body)
	}
	if code, _, body := postCAMT026ExportJSON(t, exportURL, `{"reference":"no-such-uetr","justification":"missing-or-incorrect","reason_code":"MS01"}`, "ops"); code != http.StatusNotFound {
		t.Errorf("unknown reference: status=%d body=%s, want 404", code, body)
	}
}

// The reference may be the EndToEndId: the payout ID resolves
// through the FedNow association.
func TestHTTPCamt026ExportByEndToEndID(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-camt026-exp-http-3"
	uetr := submittedPayoutHTTP(t, srv.URL, payoutID)
	exportURL := srv.URL + "/exports/camt026"

	body, err := json.Marshal(camt026ExportRequest{
		Reference:     payoutID,
		Justification: "possible-duplicate",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	code, _, xmlBody := postCAMT026ExportJSON(t, exportURL, string(body), "bank-ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/camt026: status=%d body=%s", code, xmlBody)
	}
	rep, err := ledger.ParseCAMT026([]byte(xmlBody))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	if rep.UETR != uetr || rep.EndToEndID != payoutID {
		t.Errorf("references = %q/%q, want %q/%q", rep.UETR, rep.EndToEndID, uetr, payoutID)
	}
	if rep.Justification != ledger.CAMT026JustificationPossibleDuplicate {
		t.Errorf("justification = %q, want possible-duplicate", rep.Justification)
	}
}
