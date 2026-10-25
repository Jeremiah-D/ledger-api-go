package main

import (
	"net/http"
	"strings"
	"testing"
)

// pain002ExportMsgID extracts the first GrpHdr MsgId from an exported
// document body.
func pain002ExportMsgID(t *testing.T, body string) string {
	t.Helper()
	const open, close = "<MsgId>", "</MsgId>"
	i := strings.Index(body, open)
	j := strings.Index(body, close)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("exported document has no MsgId:\n%s", body)
	}
	return body[i+len(open) : j]
}

func TestPain002ExportEndpoint(t *testing.T) {
	srv := pain001TestServer(t)

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"px1","from_account":"alice","to_account":"bob","amount_cents":12345,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("post transfer px1: status=%d", code)
	}

	code, _, pain001Doc := getBody(t, srv.URL+"/exports/pain001?currency=USD")
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pain001: status=%d", code)
	}
	msgID := pain002ExportMsgID(t, pain001Doc)

	code, ct, s := getBody(t, srv.URL+"/exports/pain002?msgid="+msgID)
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pain002: status=%d, body=%s", code, s)
	}
	if !strings.Contains(ct, "application/xml") {
		t.Errorf("Content-Type = %q, want application/xml", ct)
	}
	for _, want := range []string{
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.002.001.10">`,
		"<OrgnlMsgId>" + msgID + "</OrgnlMsgId>",
		"<OrgnlEndToEndId>px1</OrgnlEndToEndId>",
		"<TxSts>PDNG</TxSts>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("body missing %q:\n%s", want, s)
		}
	}
}

func TestPain002ExportEndpointErrors(t *testing.T) {
	srv := pain001TestServer(t)

	code, _, _ := getBody(t, srv.URL+"/exports/pain002")
	if code != http.StatusBadRequest {
		t.Errorf("missing msgid: status=%d, want 400", code)
	}

	code, _, _ = getBody(t, srv.URL+"/exports/pain002?msgid=pain001-nope")
	if code != http.StatusNotFound {
		t.Errorf("unknown msgid: status=%d, want 404", code)
	}
}
