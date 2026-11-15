package ledger

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// camt058InboundDoc renders a minimal camt.058 inbound document
// for the given schema version: the counterparty's cancellation
// advice msgID withdrawing the camt.057 notification named by
// orgnlMsgID/orgnlNtfctnID, with the free-form reason (an empty
// reason omits the CxlRsn block). Empty IDs omit their elements,
// so callers can build the missing-field shapes.
func camt058InboundDoc(t *testing.T, version, msgID, orgnlMsgID, orgnlNtfctnID, reason string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.058.` + version + `">`)
	sb.WriteString(`<NtfctnToRcvCxlAdvc><GrpHdr>`)
	if msgID != "" {
		sb.WriteString(`<MsgId>` + msgID + `</MsgId>`)
	}
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<OrgnlNtfctn>`)
	if orgnlMsgID != "" {
		sb.WriteString(`<OrgnlMsgId>` + orgnlMsgID + `</OrgnlMsgId>`)
	}
	if orgnlNtfctnID != "" {
		sb.WriteString(`<OrgnlNtfctnId>` + orgnlNtfctnID + `</OrgnlNtfctnId>`)
	}
	sb.WriteString(`</OrgnlNtfctn>`)
	if reason != "" {
		sb.WriteString(`<CxlRsn><Rsn><Prtry>` + reason + `</Prtry></Rsn></CxlRsn>`)
	}
	sb.WriteString(`</NtfctnToRcvCxlAdvc></Document>`)
	return []byte(sb.String())
}

// registerCAMT057ForCancel registers one inbound camt.057
// notification on the camt.057 fixture, the notification a later
// camt.058 advice withdraws.
func registerCAMT057ForCancel(t *testing.T, l *Ledger, msgID, ntfctnID, amount, valueDate string) ExpectedReceipt {
	t.Helper()
	doc := camt057InboundDoc(t, "001.07", msgID, ntfctnID,
		camt057IBANAcct("GB29NWBK60161331926819"), amount, "USD", valueDate,
		camt057PartyDebtor("ACME Corp"), "")
	return applyInboundCAMT057(t, l, doc, "ops").Receipt
}

// applyInboundCAMT058 parses and applies one inbound camt.058 for
// the fixture, the inbound-apply test convention.
func applyInboundCAMT058(t *testing.T, l *Ledger, doc []byte, actor string) CAMT058ApplyResult {
	t.Helper()
	req, err := ParseCAMT058(doc)
	if err != nil {
		t.Fatalf("ParseCAMT058: %v", err)
	}
	res, err := l.ApplyCAMT058(req, actor)
	if err != nil {
		t.Fatalf("ApplyCAMT058: %v", err)
	}
	return res
}

func TestParseCAMT058(t *testing.T) {
	doc := camt058InboundDoc(t, "001.08", "msg-adv-1", "msg-orig-1", "ntfctn-orig-1",
		"Client withdrew the order \u2014 duplicate notice")
	req, err := ParseCAMT058(doc)
	if err != nil {
		t.Fatalf("ParseCAMT058: %v", err)
	}
	if req.MsgID != "msg-adv-1" || req.OrgnlMsgID != "msg-orig-1" || req.OrgnlNtfctnID != "ntfctn-orig-1" {
		t.Errorf("advice = %+v, want msg-adv-1/msg-orig-1/ntfctn-orig-1", req)
	}
	if req.Reason != "Client withdrew the order \u2014 duplicate notice" {
		t.Errorf("reason = %q, want the verbatim reason", req.Reason)
	}
	// No CxlRsn block: the reason reads as "".
	doc = camt058InboundDoc(t, "001.08", "msg-adv-2", "msg-orig-2", "ntfctn-orig-2", "")
	req, err = ParseCAMT058(doc)
	if err != nil {
		t.Fatalf("ParseCAMT058 no-reason: %v", err)
	}
	if req.Reason != "" {
		t.Errorf("reason = %q, want empty", req.Reason)
	}
}

func TestParseCAMT058Rejects(t *testing.T) {
	cases := []struct {
		name string
		doc  []byte
	}{
		{"empty document", []byte("  ")},
		{"malformed XML", []byte("<Document><NtfctnToRcvCxlAdvc>")},
		{"wrong version", camt058InboundDoc(t, "001.07", "msg-adv-r", "msg-orig-r", "ntfctn-orig-r", "reason")},
		{"missing MsgId", camt058InboundDoc(t, "001.08", "", "msg-orig-r", "ntfctn-orig-r", "reason")},
		{"missing OrgnlMsgId", camt058InboundDoc(t, "001.08", "msg-adv-r", "", "ntfctn-orig-r", "reason")},
		{"missing OrgnlNtfctnId", camt058InboundDoc(t, "001.08", "msg-adv-r", "msg-orig-r", "", "reason")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseCAMT058(tc.doc); err == nil {
				t.Fatal("ParseCAMT058: got nil error, want a refusal")
			} else if !errors.Is(err, ErrInvalidCAMT058) {
				t.Errorf("error = %v, want ErrInvalidCAMT058", err)
			}
		})
	}
}

func TestApplyCAMT058(t *testing.T) {
	l, al := camt057Fixture(t)
	registerCAMT057ForCancel(t, l, "msg-cxl-1", "ntfctn-cxl-1", "250.00", "2026-11-20")
	versionBefore := l.Version()
	doc := camt058InboundDoc(t, "001.08", "msg-adv-1", "msg-cxl-1", "ntfctn-cxl-1",
		"Client withdrew the order \u2014 duplicate notice")
	res := applyInboundCAMT058(t, l, doc, "ops")
	if res.Duplicate {
		t.Error("duplicate = true on first apply, want false")
	}
	r := res.Receipt
	if !r.Cancelled || r.CancelMsgID != "msg-adv-1" {
		t.Errorf("receipt cancelled = %v/%q, want true/msg-adv-1", r.Cancelled, r.CancelMsgID)
	}
	if r.CancelReason != "Client withdrew the order \u2014 duplicate notice" {
		t.Errorf("cancel reason = %q, want the verbatim reason", r.CancelReason)
	}
	if r.CancelledAt.IsZero() {
		t.Error("cancelled_at is zero")
	}
	// The notification's own fields are preserved, not rewritten.
	if r.MsgID != "msg-cxl-1" || r.NotificationID != "ntfctn-cxl-1" {
		t.Errorf("receipt ids = %q/%q, want msg-cxl-1/ntfctn-cxl-1", r.MsgID, r.NotificationID)
	}
	if r.Account != "bob" || r.AmountCents != 25000 || r.Currency != "USD" {
		t.Errorf("receipt = %+v, want account bob 25000 USD", r)
	}
	if r.ExpectedValueDate != "2026-11-20" || r.Debtor != "ACME Corp" {
		t.Errorf("receipt = %+v, want 2026-11-20/ACME Corp", r)
	}
	// Only the forecast changes: no journal rows, no version bump.
	if l.Version() != versionBefore {
		t.Errorf("version = %d, want %d (no bump)", l.Version(), versionBefore)
	}
	if len(l.Entries()) != 0 {
		t.Errorf("entries = %d, want 0", len(l.Entries()))
	}
	// The stored receipt reads back cancelled, and the audit
	// event is precise.
	got, ok := l.GetExpectedReceipt("msg-cxl-1")
	if !ok || !got.Cancelled || got.CancelMsgID != "msg-adv-1" || got.CancelReason != "Client withdrew the order \u2014 duplicate notice" {
		t.Errorf("GetExpectedReceipt = %+v/%v, want cancelled msg-adv-1 with the verbatim reason", got, ok)
	}
	ev := findOp(readAuditOps(t, al), "camt057_notification_cancelled")
	if ev == nil {
		t.Fatal("no camt057_notification_cancelled audit event")
	}
	if ev.Details["msg_id"] != "msg-adv-1" || ev.Details["orgnl_msg_id"] != "msg-cxl-1" {
		t.Errorf("audit details = %+v, want msg_id msg-adv-1 orgnl_msg_id msg-cxl-1", ev.Details)
	}
	if ev.Details["reason"] != "Client withdrew the order \u2014 duplicate notice" {
		t.Errorf("audit details = %+v, want the verbatim reason", ev.Details)
	}
	if countOp(readAuditOps(t, al), "camt057_notification_cancelled") != 1 {
		t.Error("expected exactly one cancellation audit event")
	}
}

func TestApplyCAMT058UnknownNotification(t *testing.T) {
	l, al := camt057Fixture(t)
	doc := camt058InboundDoc(t, "001.08", "msg-adv-unk", "msg-never", "ntfctn-never", "reason")
	req, err := ParseCAMT058(doc)
	if err != nil {
		t.Fatalf("ParseCAMT058: %v", err)
	}
	res, err := l.ApplyCAMT058(req, "ops")
	if !errors.Is(err, ErrCAMT058UnknownNotification) {
		t.Fatalf("ApplyCAMT058 = %v, want ErrCAMT058UnknownNotification", err)
	}
	if res.RepairID == "" {
		t.Error("repair_id is empty on the parked unknown-original path")
	}
	// The ledger never invents a notification in order to cancel
	// it: nothing is registered under the named original.
	if _, ok := l.GetExpectedReceipt("msg-never"); ok {
		t.Error("unknown original was registered")
	}
	if countOp(readAuditOps(t, al), "camt057_notification_cancelled") != 0 {
		t.Error("unknown-original refusal emitted a cancellation audit event")
	}
}

func TestApplyCAMT058NotificationIDMismatch(t *testing.T) {
	l, _ := camt057Fixture(t)
	registerCAMT057ForCancel(t, l, "msg-cxl-2", "ntfctn-cxl-2", "250.00", "2026-11-20")
	// The advice names the right original MsgId but the wrong
	// notification Id: it does not name the same notification
	// both ways.
	doc := camt058InboundDoc(t, "001.08", "msg-adv-2", "msg-cxl-2", "ntfctn-other", "reason")
	req, err := ParseCAMT058(doc)
	if err != nil {
		t.Fatalf("ParseCAMT058: %v", err)
	}
	res, err := l.ApplyCAMT058(req, "ops")
	if !errors.Is(err, ErrInvalidCAMT058) {
		t.Fatalf("ApplyCAMT058 = %v, want ErrInvalidCAMT058", err)
	}
	if res.RepairID == "" {
		t.Error("repair_id is empty on the parked mismatch path")
	}
	got, ok := l.GetExpectedReceipt("msg-cxl-2")
	if !ok || got.Cancelled {
		t.Errorf("receipt = %+v/%v, want registered and not cancelled", got, ok)
	}
}

func TestApplyCAMT058DuplicateAndConflict(t *testing.T) {
	l, al := camt057Fixture(t)
	registerCAMT057ForCancel(t, l, "msg-cxl-3", "ntfctn-cxl-3", "250.00", "2026-11-20")
	doc := camt058InboundDoc(t, "001.08", "msg-adv-3", "msg-cxl-3", "ntfctn-cxl-3", "reason")
	first := applyInboundCAMT058(t, l, doc, "ops")
	second := applyInboundCAMT058(t, l, doc, "ops")
	if !second.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if !reflect.DeepEqual(second.Receipt, first.Receipt) {
		t.Errorf("replay receipt = %+v, want %+v", second.Receipt, first.Receipt)
	}
	if countOp(readAuditOps(t, al), "camt057_notification_cancelled") != 1 {
		t.Error("replay emitted a second audit event")
	}
	// The same advice MsgId for a different original is a
	// conflict.
	registerCAMT057ForCancel(t, l, "msg-cxl-3b", "ntfctn-cxl-3b", "100.00", "2026-11-21")
	other := camt058InboundDoc(t, "001.08", "msg-adv-3", "msg-cxl-3b", "ntfctn-cxl-3b", "reason")
	req, err := ParseCAMT058(other)
	if err != nil {
		t.Fatalf("ParseCAMT058: %v", err)
	}
	if _, err := l.ApplyCAMT058(req, "ops"); !errors.Is(err, ErrCAMT058MsgIDConflict) {
		t.Errorf("ApplyCAMT058 conflict = %v, want ErrCAMT058MsgIDConflict", err)
	}
}

func TestApplyCAMT058SecondAdviceDuplicate(t *testing.T) {
	l, al := camt057Fixture(t)
	registerCAMT057ForCancel(t, l, "msg-cxl-4", "ntfctn-cxl-4", "250.00", "2026-11-20")
	firstDoc := camt058InboundDoc(t, "001.08", "msg-adv-4a", "msg-cxl-4", "ntfctn-cxl-4", "first reason")
	first := applyInboundCAMT058(t, l, firstDoc, "ops")
	// A different advice MsgId cancelling an already-cancelled
	// notification: a no-op success returning the stored
	// (cancelled) receipt, with the first advice still named as
	// the canceller.
	secondDoc := camt058InboundDoc(t, "001.08", "msg-adv-4b", "msg-cxl-4", "ntfctn-cxl-4", "second reason")
	second := applyInboundCAMT058(t, l, secondDoc, "ops")
	if !second.Duplicate {
		t.Error("duplicate = false on second advice, want true")
	}
	if !reflect.DeepEqual(second.Receipt, first.Receipt) {
		t.Errorf("second-advice receipt = %+v, want %+v", second.Receipt, first.Receipt)
	}
	if second.Receipt.CancelMsgID != "msg-adv-4a" || second.Receipt.CancelReason != "first reason" {
		t.Errorf("receipt = %+v, want canceller msg-adv-4a/first reason", second.Receipt)
	}
	if countOp(readAuditOps(t, al), "camt057_notification_cancelled") != 1 {
		t.Error("second advice emitted a second audit event")
	}
	// The second advice's own replay converges the same way.
	third := applyInboundCAMT058(t, l, secondDoc, "ops")
	if !third.Duplicate || !reflect.DeepEqual(third.Receipt, first.Receipt) {
		t.Errorf("second-advice replay = %+v, want duplicate of %+v", third.Receipt, first.Receipt)
	}
}

func TestIntradayLiquidityCancelledNotification(t *testing.T) {
	l, _ := camt057Fixture(t)
	registerCAMT057ForCancel(t, l, "msg-liq-a", "ntfctn-liq-a", "250.00", "2026-11-20")
	registerCAMT057ForCancel(t, l, "msg-liq-b", "ntfctn-liq-b", "100.00", "2026-11-20")
	registerCAMT057ForCancel(t, l, "msg-liq-c", "ntfctn-liq-c", "50.00", "2026-11-21")
	doc := camt058InboundDoc(t, "001.08", "msg-adv-liq", "msg-liq-a", "ntfctn-liq-a", "withdrawn")
	applyInboundCAMT058(t, l, doc, "ops")

	day := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	pos := liquidityPos(t, l.IntradayLiquidity(day), "bob", "USD")
	if pos.NotifiedInflowCents != 10000 {
		t.Errorf("notified inflow = %d, want 10000 (cancelled 25000 excluded)", pos.NotifiedInflowCents)
	}
	if pos.NetPositionCents != 10000 {
		t.Errorf("net = %d, want 10000", pos.NetPositionCents)
	}
	// The other day's notification is untouched.
	otherDay := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	other := liquidityPos(t, l.IntradayLiquidity(otherDay), "bob", "USD")
	if other.NotifiedInflowCents != 5000 {
		t.Errorf("other-day notified inflow = %d, want 5000", other.NotifiedInflowCents)
	}
}

func TestParkCAMT058Unparsed(t *testing.T) {
	l, _ := camt057Fixture(t)
	// A wrong-version document that still names its MsgId parks.
	doc := camt058InboundDoc(t, "001.07", "msg-park-1", "msg-orig-park-1", "ntfctn-park-1", "reason")
	if _, err := ParseCAMT058(doc); err == nil {
		t.Fatal("ParseCAMT058 accepted a 001.07 document")
	}
	id, err := l.ParkCAMT058Unparsed(doc, "ops")
	if err != nil {
		t.Fatalf("ParkCAMT058Unparsed: %v", err)
	}
	if id == "" {
		t.Error("repair_id is empty for a named unparsed document")
	}
	// A document naming only its original parks on the
	// OrgnlMsgId.
	doc = camt058InboundDoc(t, "001.07", "", "msg-orig-park-2", "ntfctn-park-2", "reason")
	id, err = l.ParkCAMT058Unparsed(doc, "ops")
	if err != nil {
		t.Fatalf("ParkCAMT058Unparsed original-only: %v", err)
	}
	if id == "" {
		t.Error("repair_id is empty for an original-named unparsed document")
	}
	// Garbage names nothing: nothing to park.
	id, err = l.ParkCAMT058Unparsed([]byte("not xml at all"), "ops")
	if err != nil {
		t.Fatalf("ParkCAMT058Unparsed garbage: %v", err)
	}
	if id != "" {
		t.Errorf("repair_id = %q for garbage, want empty", id)
	}
}
