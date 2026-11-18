package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// mt292ApplyDoc renders a minimal inbound MT292 FIN text for the
// endpoint tests: the counterparty's cancellation request with
// :20: msgID, :21: relatedMsgID (the original MT210's :20:),
// :11S: orig11S, and :79: reason.
func mt292ApplyDoc(msgID, relatedMsgID, orig11S, reason string) string {
	return "{1:F01NWBKGB2LXXX0000000000}\n" +
		"{2:I292CITIUS33XXXU3003}\n" +
		"{3:{108:" + msgID + "}}\n" +
		"{4:\n" +
		":20:" + msgID + "\n" +
		":21:" + relatedMsgID + "\n" +
		":11S:" + orig11S + "\n" +
		":79:" + reason + "\n" +
		"-}"
}

// postMT292JSON posts a {"mt292": doc} JSON body to
// /exports/mt292/apply with an x-ledger-actor header.
func postMT292JSON(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"mt292": finDoc})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
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

// postMT292Raw posts a raw body to /exports/mt292/apply (the
// malformed-JSON shape).
func postMT292Raw(t *testing.T, url, raw, actor string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(raw))
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

// mt292DocRef extracts the :20: reference from an MT292 FIN text.
func mt292DocRef(t *testing.T, doc string) string {
	t.Helper()
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, ":20:") {
			return line[len(":20:"):]
		}
	}
	t.Fatal("no :20: in MT292 document")
	return ""
}

// Full flow through HTTP: export the MT210 notice, apply it back,
// export the MT292 cancellation request for the paused schedule,
// apply it back, and the metrics counter moves.
func TestMT292ApplyEndpoint_CancelsAndReturnsReceipt(t *testing.T) {
	l, sched := mt210HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/mt292/apply"

	// Register the counterparty's MT210: this ledger's own
	// export, sent back verbatim.
	q := url.Values{}
	q.Set("schedule_id", sched.ID)
	q.Set("servicing_bic", "CITIUS33")
	code, doc, _ := getText(t, srv.URL+"/exports/mt210?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export MT210: status=%d, want 200", code)
	}
	code, body := postMT210Text(t, srv.URL+"/exports/mt210/apply", doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt210/apply: status=%d body=%v", code, body)
	}

	// Pause the schedule, then export the MT292 cancellation
	// request for it — the same document the counterparty would
	// send back to withdraw the notice.
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	q = url.Values{}
	q.Set("schedule_id", sched.ID)
	q.Set("servicing_bic", "CITIUS33")
	q.Set("original_date", "2026-11-10")
	q.Set("reason", "Schedule paused by operator")
	code, doc292, _ := getText(t, srv.URL+"/exports/mt292?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export MT292: status=%d, want 200", code)
	}
	ref := mt292DocRef(t, doc292)

	code, body = postMT292JSON(t, applyURL, doc292, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt292/apply: status=%d body=%v", code, body)
	}
	receipt, ok := body["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt = %v, want an object", body["receipt"])
	}
	if receipt["msg_id"] != "sched-1" {
		t.Errorf("receipt msg_id = %v, want sched-1", receipt["msg_id"])
	}
	if receipt["cancelled"] != true {
		t.Errorf("receipt cancelled = %v, want true", receipt["cancelled"])
	}
	if receipt["cancel_msg_id"] != ref {
		t.Errorf("receipt cancel_msg_id = %v, want %s", receipt["cancel_msg_id"], ref)
	}
	if receipt["cancel_reason"] != "Schedule paused by operator" {
		t.Errorf("receipt cancel_reason = %v, want the verbatim reason", receipt["cancel_reason"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A malformed JSON body is a 400.
	code, body = postMT292Raw(t, applyURL, "not json", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status=%d body=%v, want 400", code, body)
	}
	// A malformed FIN document is a 400 with no repair record: it
	// names no notification.
	code, body = postMT292JSON(t, applyURL, "{1:F01DEUTDEFFXXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] != nil {
		t.Errorf("malformed repair_id = %v, want none (no :21: named)", body["repair_id"])
	}
	// A document that names a :21: but fails the parse is a 400
	// with the repair record ID: the request is parked, not dropped.
	broken := strings.Replace(mt292ApplyDoc("MT292-0123456789", "sched-1", "210261110", "reason"), ":11S:210261110\n", "", 1)
	code, body = postMT292JSON(t, applyURL, broken, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("broken: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("broken repair_id = %v, want the parked repair record", body["repair_id"])
	}
	// An unknown :21: original is a 422 with the repair record
	// ID: the ledger never invents a notification to cancel.
	unknown := mt292ApplyDoc("MT292-9999999999", "sched-9", "210261110", "reason")
	code, body = postMT292JSON(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown original: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("unknown repair_id = %v, want the parked repair record", body["repair_id"])
	}
	// Replay the same :20: with the same original: idempotent,
	// duplicate set.
	code, body = postMT292JSON(t, applyURL, doc292, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt292_applies_total 6") {
		t.Errorf("metrics exposition missing ledger_mt292_applies_total 6:\n%s", metricsBody)
	}
}
