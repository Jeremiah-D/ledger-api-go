package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestEntryMetadataHTTPLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// POST /entries with metadata: 201, echoed on the entry.
	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"a","credit_account":"b","amount_cents":100,
		  "metadata":{"channel":"alipay","risk_score":"12"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries: status=%d, want 201 (body=%v)", code, body)
	}
	meta, ok := body["metadata"].(map[string]any)
	if !ok || meta["channel"] != "alipay" || meta["risk_score"] != "12" {
		t.Errorf("metadata = %v, want channel=alipay risk_score=12", body["metadata"])
	}

	// Invalid metadata is 400: bad key, too many pairs, long value.
	for _, payload := range []string{
		`{"debit_account":"a","credit_account":"b","amount_cents":100,"metadata":{"bad key":"v"}}`,
		`{"debit_account":"a","credit_account":"b","amount_cents":100,"metadata":{"` + repeatStr("k", 65) + `":"v"}}`,
	} {
		code, _ = postJSON(t, srv.URL+"/entries", payload)
		if code != http.StatusBadRequest {
			t.Errorf("invalid metadata: status=%d, want 400 (payload=%s)", code, payload)
		}
	}
	many := `"k0":"v"`
	for i := 1; i < 17; i++ {
		many += fmt.Sprintf(`,"k%d":"v"`, i)
	}
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"a","credit_account":"b","amount_cents":100,"metadata":{`+many+`}}`)
	if code != http.StatusBadRequest {
		t.Errorf("17 pairs: status=%d, want 400", code)
	}

	// Idempotent replay returns the entry with its metadata.
	code, body = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"x","credit_account":"y","amount_cents":50,
		  "idempotency_key":"meta-h-key","metadata":{"channel":"wechat"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST with key: status=%d, want 201", code)
	}
	code, replay := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"x","credit_account":"y","amount_cents":50,
		  "idempotency_key":"meta-h-key","metadata":{"channel":"other"}}`)
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d, want 200", code)
	}
	if replay["id"] != body["id"] {
		t.Errorf("replay id = %v, want %v", replay["id"], body["id"])
	}
	rmeta, ok := replay["metadata"].(map[string]any)
	if !ok || rmeta["channel"] != "wechat" {
		t.Errorf("replay metadata = %v, want the original channel=wechat", replay["metadata"])
	}
}

func repeatStr(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestEntryMetadataFilterHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	post := func(memo, meta string) {
		t.Helper()
		code, _ := postJSON(t, srv.URL+"/entries",
			`{"debit_account":"a","credit_account":"b","amount_cents":100,"memo":`+fmt.Sprintf("%q", memo)+`,"metadata":{`+meta+`}}`)
		if code != http.StatusCreated {
			t.Fatalf("POST /entries: status=%d, want 201", code)
		}
	}
	post("order-1", `"channel":"alipay","risk_score":"12"`)
	post("order-2", `"channel":"wechat"`)
	post("order-3", `"channel":"alipay"`)

	// ?meta.channel=alipay filters.
	code, body := getJSON(t, srv.URL+"/entries?meta.channel=alipay")
	if code != http.StatusOK {
		t.Fatalf("GET /entries: status=%d, want 200", code)
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("meta filter = %d entries, want 2", len(entries))
	}

	// Composes with ?memo= and pagination: limit=1 pages through the
	// two matching entries via next_cursor.
	code, body = getJSON(t, srv.URL+"/entries?meta.channel=alipay&memo=order&limit=1")
	if code != http.StatusOK {
		t.Fatalf("GET filtered page 1: status=%d, want 200", code)
	}
	if len(body["entries"].([]any)) != 1 {
		t.Fatalf("page 1 = %d entries, want 1", len(body["entries"].([]any)))
	}
	cursor, _ := body["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("page 1 next_cursor empty, want a cursor")
	}
	code, body = getJSON(t, srv.URL+"/entries?meta.channel=alipay&memo=order&limit=1&cursor="+cursor)
	if code != http.StatusOK {
		t.Fatalf("GET filtered page 2: status=%d, want 200", code)
	}
	if len(body["entries"].([]any)) != 1 {
		t.Errorf("page 2 = %d entries, want 1", len(body["entries"].([]any)))
	}
	if body["next_cursor"] != "" {
		t.Errorf("page 2 next_cursor = %v, want empty (last page)", body["next_cursor"])
	}

	// A pair no entry carries matches nothing.
	code, body = getJSON(t, srv.URL+"/entries?meta.channel=unionpay")
	if code != http.StatusOK {
		t.Fatalf("GET unmatched: status=%d, want 200", code)
	}
	if len(body["entries"].([]any)) != 0 {
		t.Errorf("unmatched = %d entries, want 0", len(body["entries"].([]any)))
	}
}

func TestTransferMetadataHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"meta-t-1","from_account":"a","to_account":"b","amount_cents":1000,
		  "fee_cents":50,"fee_account":"fees","skip_fee":false,
		  "metadata":{"channel":"alipay"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /transfers: status=%d, want 201 (body=%v)", code, body)
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	principal := entries[0].(map[string]any)
	pmeta, ok := principal["metadata"].(map[string]any)
	if !ok || pmeta["channel"] != "alipay" {
		t.Errorf("principal metadata = %v, want channel=alipay", principal["metadata"])
	}
	fee := entries[1].(map[string]any)
	if _, has := fee["metadata"]; has {
		t.Errorf("fee leg metadata = %v, want absent", fee["metadata"])
	}

	// Invalid transfer metadata: 400.
	code, _ = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"meta-t-2","from_account":"a","to_account":"b","amount_cents":100,
		  "metadata":{"bad key":"v"}}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad transfer metadata: status=%d, want 400", code)
	}
}

func TestReversalMetadataHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"a","credit_account":"b","amount_cents":500,
		  "metadata":{"channel":"alipay"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries: status=%d, want 201", code)
	}
	id, _ := body["id"].(string)

	// POST /entries/{id}/reverse with metadata: the reversal carries
	// only the caller's metadata, not the original's.
	code, rev := postJSON(t, srv.URL+"/entries/"+id+"/reverse",
		`{"metadata":{"reason":"fraud"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /reverse: status=%d, want 201 (body=%v)", code, rev)
	}
	rmeta, ok := rev["metadata"].(map[string]any)
	if !ok || rmeta["reason"] != "fraud" || len(rmeta) != 1 {
		t.Errorf("reversal metadata = %v, want only reason=fraud", rev["metadata"])
	}

	// Invalid reversal metadata: 400.
	code, _ = postJSON(t, srv.URL+"/entries/"+id+"/reverse",
		`{"metadata":{"bad key":"v"}}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad reversal metadata: status=%d, want 400", code)
	}
}

func TestCaptureMetadataHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"card","credit_account":"funding","amount_cents":6000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding: status=%d, want 201", code)
	}
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, _ = postJSON(t, srv.URL+"/holds",
		fmt.Sprintf(`{"hold_id":"meta-h1","account":"card","amount_cents":6000,"expires_at":%q}`, expiry))
	if code != http.StatusCreated {
		t.Fatalf("POST /holds: status=%d, want 201", code)
	}

	// Capture with metadata: the settlement entry carries it.
	code, receipt := postJSON(t, srv.URL+"/holds/meta-h1/capture",
		`{"capture_id":"meta-c1","to_account":"merchant","amount_cents":4000,
		  "metadata":{"channel":"alipay"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /capture: status=%d, want 201 (body=%v)", code, receipt)
	}
	entry, ok := receipt["entry"].(map[string]any)
	if !ok {
		t.Fatalf("capture receipt has no entry: %v", receipt)
	}
	emeta, ok := entry["metadata"].(map[string]any)
	if !ok || emeta["channel"] != "alipay" {
		t.Errorf("settlement metadata = %v, want channel=alipay", entry["metadata"])
	}

	// Invalid capture metadata: 400.
	code, _ = postJSON(t, srv.URL+"/holds/meta-h1/capture",
		`{"capture_id":"meta-c2","to_account":"merchant","amount_cents":100,
		  "metadata":{"bad key":"v"}}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad capture metadata: status=%d, want 400", code)
	}
}

func TestBatchMetadataHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/entries/batch",
		`{"batch_id":"meta-batch-h1","entries":[
		  {"entry_id":"meta-hb-1","debit_account":"a","credit_account":"b","amount_cents":100,
		   "metadata":{"channel":"alipay"}},
		  {"entry_id":"meta-hb-2","debit_account":"b","credit_account":"c","amount_cents":100}
		]}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries/batch: status=%d, want 201 (body=%v)", code, body)
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	first := entries[0].(map[string]any)
	fmeta, ok := first["metadata"].(map[string]any)
	if !ok || fmeta["channel"] != "alipay" {
		t.Errorf("batch entry metadata = %v, want channel=alipay", first["metadata"])
	}
}

func TestDryRunMetadataHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"a","credit_account":"funding","amount_cents":10000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding: status=%d, want 201", code)
	}

	// POST /transfers/dry-run echoes the metadata on the simulated legs.
	code, body := postJSON(t, srv.URL+"/transfers/dry-run",
		`{"transfer_id":"meta-dr-h1","from_account":"a","to_account":"b","amount_cents":1000,
		  "skip_fee":true,"metadata":{"channel":"alipay"}}`)
	if code != http.StatusOK {
		t.Fatalf("POST /transfers/dry-run: status=%d, want 200 (body=%v)", code, body)
	}
	legs, ok := body["legs"].([]any)
	if !ok || len(legs) == 0 {
		t.Fatalf("dry-run legs = %v, want non-empty", body["legs"])
	}
	lmeta, ok := legs[0].(map[string]any)["metadata"].(map[string]any)
	if !ok || lmeta["channel"] != "alipay" {
		t.Errorf("dry-run leg metadata = %v, want channel=alipay", legs[0].(map[string]any)["metadata"])
	}
	// Nothing was recorded: the dry run stayed a simulation.
	code, body = getJSON(t, srv.URL+"/entries?meta.channel=alipay")
	if code != http.StatusOK {
		t.Fatalf("GET /entries: status=%d, want 200", code)
	}
	if len(body["entries"].([]any)) != 0 {
		t.Errorf("journal has %d entries with channel=alipay after dry run, want 0",
			len(body["entries"].([]any)))
	}
}
