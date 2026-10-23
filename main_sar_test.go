package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func seedSARServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New(ledger.WithLargeTxThresholds(map[string]int64{"USD": 1000000}))
	now := time.Now()
	// "suspect" runs the structuring pattern: three 8,000.00 inflows
	// just under the 10,000.00 reporting threshold.
	for i := 0; i < 3; i++ {
		if _, _, err := l.Post(ledger.JournalEntry{
			ID:            fmt.Sprintf("sus-%d", i),
			DebitAccount:  "suspect",
			CreditAccount: "equity",
			AmountCents:   800000,
			Currency:      "USD",
			CreatedAt:     now.Add(-time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed post: %v", err)
		}
	}
	// "clean" has one small ordinary posting.
	if _, _, err := l.Post(ledger.JournalEntry{
		ID:            "clean-1",
		DebitAccount:  "clean",
		CreditAccount: "equity",
		AmountCents:   5000,
		Currency:      "USD",
		CreatedAt:     now,
	}); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

func sarWindowJSON() string {
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	since := time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	return fmt.Sprintf(`{"account":"%s","since":%q,"until":%q}`, "ACCOUNT", since, until)
}

func TestHTTPSARFlow(t *testing.T) {
	srv := seedSARServer(t)
	defer srv.Close()

	window := func(account string) string {
		return strings.Replace(sarWindowJSON(), "ACCOUNT", account, 1)
	}

	// Draft for the suspicious account: 201 with the structuring reason.
	code, draft := postJSON(t, srv.URL+"/compliance/sar/draft", window("suspect"))
	if code != http.StatusCreated {
		t.Fatalf("draft status = %d, want 201 (body %v)", code, draft)
	}
	id, _ := draft["id"].(string)
	if !strings.HasPrefix(id, "SAR-") {
		t.Fatalf("draft id = %v, want SAR-NNNN", draft["id"])
	}
	if draft["status"] != "draft" {
		t.Errorf("draft status = %v, want draft", draft["status"])
	}
	codes, _ := draft["reason_codes"].([]any)
	found := false
	for _, c := range codes {
		if c == "STRUCTURING" {
			found = true
		}
	}
	if !found {
		t.Errorf("reason_codes = %v, want STRUCTURING", codes)
	}

	// Clean account: 422, no suspicious activity.
	code, body := postJSON(t, srv.URL+"/compliance/sar/draft", window("clean"))
	if code != http.StatusUnprocessableEntity {
		t.Errorf("clean draft status = %d, want 422 (body %v)", code, body)
	}

	// Bad window: 400.
	code, body = postJSON(t, srv.URL+"/compliance/sar/draft",
		`{"account":"suspect","since":"2026-10-08T00:00:00Z","until":"2026-10-01T00:00:00Z"}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad window status = %d, want 400 (body %v)", code, body)
	}

	// Fetch the draft.
	code, got := getJSON(t, srv.URL+"/compliance/sar/"+id)
	if code != http.StatusOK || got["id"] != id {
		t.Errorf("get draft: status=%d body=%v", code, got)
	}

	// File it: 200, filed, content hash sealed.
	code, filed := postJSON(t, srv.URL+"/compliance/sar/"+id+"/file", "")
	if code != http.StatusOK {
		t.Fatalf("file status = %d, want 200 (body %v)", code, filed)
	}
	if filed["status"] != "filed" {
		t.Errorf("filed status = %v, want filed", filed["status"])
	}
	if filed["content_hash"] == nil || filed["content_hash"] == "" {
		t.Errorf("filed report has no content_hash: %v", filed)
	}
	if filed["filed_at"] == nil {
		t.Errorf("filed report has no filed_at: %v", filed)
	}

	// Refiling is rejected: filed reports are immutable.
	code, body = postJSON(t, srv.URL+"/compliance/sar/"+id+"/file", "")
	if code != http.StatusConflict {
		t.Errorf("refile status = %d, want 409 (body %v)", code, body)
	}

	// Unknown report: 404 on get and file.
	code, _ = getJSON(t, srv.URL+"/compliance/sar/SAR-9999")
	if code != http.StatusNotFound {
		t.Errorf("get unknown status = %d, want 404", code)
	}
	code, _ = postJSON(t, srv.URL+"/compliance/sar/SAR-9999/file", "")
	if code != http.StatusNotFound {
		t.Errorf("file unknown status = %d, want 404", code)
	}

	// List shows the filed report.
	code, list := getJSON(t, srv.URL+"/compliance/sar")
	if code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", code)
	}
	reports, _ := list["reports"].([]any)
	if len(reports) != 1 {
		t.Errorf("list reports = %d, want 1", len(reports))
	}

}

func TestHTTPSARMetrics(t *testing.T) {
	srv := seedSARServer(t)
	defer srv.Close()

	window := func(account string) string {
		return strings.Replace(sarWindowJSON(), "ACCOUNT", account, 1)
	}
	code, draft := postJSON(t, srv.URL+"/compliance/sar/draft", window("suspect"))
	if code != http.StatusCreated {
		t.Fatalf("draft status = %d", code)
	}
	id := draft["id"].(string)
	if code, _ := postJSON(t, srv.URL+"/compliance/sar/"+id+"/file", ""); code != http.StatusOK {
		t.Fatalf("file status = %d", code)
	}

	code, text, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d", code)
	}
	if !strings.Contains(text, "ledger_sar_drafts_total 1") {
		t.Errorf("metrics missing ledger_sar_drafts_total 1")
	}
	if !strings.Contains(text, "ledger_sar_filed_total 1") {
		t.Errorf("metrics missing ledger_sar_filed_total 1")
	}
}
